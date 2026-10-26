// Package fcm envía notificaciones push por Firebase Cloud Messaging (API
// HTTP v1), autenticándose con la cuenta de servicio del proyecto: firma un
// JWT con su clave privada y lo cambia por un access token OAuth2, que se
// reutiliza hasta poco antes de vencer (igual que App\Services\FcmService).
package fcm

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Resultado int

const (
	OK Resultado = iota
	TokenInvalido
	Error
)

type credenciales struct {
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

type Cliente struct {
	cred  credenciales
	clave *rsa.PrivateKey
	http  *http.Client

	mu    sync.Mutex
	token string
	vence time.Time
}

func NuevoCliente(ruta string) (*Cliente, error) {
	if ruta == "" {
		return nil, fmt.Errorf("no se configuró FCM_CREDENTIALS")
	}
	b, err := os.ReadFile(ruta)
	if err != nil {
		return nil, fmt.Errorf("no se encontró el archivo de credenciales de Firebase (%s)", ruta)
	}
	var c credenciales
	if err := json.Unmarshal(b, &c); err != nil || c.PrivateKey == "" || c.ProjectID == "" {
		return nil, fmt.Errorf("el archivo de credenciales de Firebase no es una cuenta de servicio válida")
	}
	if c.TokenURI == "" {
		c.TokenURI = "https://oauth2.googleapis.com/token"
	}

	bloque, _ := pem.Decode([]byte(c.PrivateKey))
	if bloque == nil {
		return nil, fmt.Errorf("clave privada de Firebase inválida")
	}
	var rsaKey *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(bloque.Bytes); err == nil {
		var ok bool
		if rsaKey, ok = k.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("la clave privada de Firebase no es RSA")
		}
	} else if rsaKey, err = x509.ParsePKCS1PrivateKey(bloque.Bytes); err != nil {
		return nil, fmt.Errorf("clave privada de Firebase inválida: %w", err)
	}

	return &Cliente{cred: c, clave: rsaKey, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

// Enviar manda una notificación a un token. TokenInvalido significa que la
// app se desinstaló o el token rotó: hay que borrarlo.
func (c *Cliente) Enviar(ctx context.Context, token, titulo, mensaje string, data map[string]string) Resultado {
	access, err := c.accessToken(ctx)
	if err != nil {
		return Error
	}

	cuerpo, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"token":        token,
			"notification": map[string]string{"title": titulo, "body": mensaje},
			"data":         data,
			"android":      map[string]string{"priority": "high"},
			"apns":         map[string]any{"payload": map[string]any{"aps": map[string]string{"sound": "default"}}},
		},
	})

	url := "https://fcm.googleapis.com/v1/projects/" + c.cred.ProjectID + "/messages:send"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(cuerpo))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Error
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return OK
	}

	var r struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = json.Unmarshal(b, &r)

	if resp.StatusCode == http.StatusNotFound || r.Error.Status == "UNREGISTERED" ||
		(r.Error.Status == "INVALID_ARGUMENT" && strings.Contains(strings.ToLower(r.Error.Message), "registration token")) {
		return TokenInvalido
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
	}
	return Error
}

func (c *Cliente) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.vence) {
		return c.token, nil
	}

	ahora := time.Now().Unix()
	cabecera := b64url(mustJSON(map[string]string{"alg": "RS256", "typ": "JWT"}))
	carga := b64url(mustJSON(map[string]any{
		"iss":   c.cred.ClientEmail,
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud":   c.cred.TokenURI,
		"iat":   ahora,
		"exp":   ahora + 3600,
	}))
	firmar := cabecera + "." + carga
	h := sha256.Sum256([]byte(firmar))
	firma, err := rsa.SignPKCS1v15(nil, c.clave, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("no se pudo firmar el JWT de Firebase: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {firmar + "." + b64url(firma)},
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.cred.TokenURI, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Firebase rechazó las credenciales: %s", b)
	}
	var r struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.AccessToken == "" {
		return "", fmt.Errorf("respuesta de token de Firebase inválida")
	}

	c.token = r.AccessToken
	c.vence = time.Now().Add(3300 * time.Second)
	return c.token, nil
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
