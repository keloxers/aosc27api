// Package config lee la configuración del servicio desde variables de
// entorno (ver .env.example).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"aosc_api/internal/instancia"
)

type Config struct {
	// Instancia (aosc | covesa): valores propios de cada una.
	Instancia *instancia.Perfil

	HTTPAddr string

	DBHost     string
	DBPort     string
	DBDatabase string
	DBUsername string
	DBPassword string

	JWTSecret []byte
	JWTTTL    time.Duration

	MailHost       string
	MailPort       string
	MailUsername   string
	MailPassword   string
	MailEncryption string // "tls" (STARTTLS), "ssl" (TLS implícito) o vacío
	MailFrom       string
	MailFromName   string

	// URL pública del portal donde se valida el correo, sin barra final
	// (el mail arma {ValidarCorreoURL}/{codigo}).
	ValidarCorreoURL string

	// URL pública de los archivos de MinIO (endpoint + bucket), de donde se
	// descarga la plantilla de la factura.
	ArchivosURL string

	// Si es false no se llama a Wee (ni checkout ni QR): útil en local.
	WeeHabilitado bool

	// Envío de push. Arranca apagado para no duplicar avisos mientras siga
	// activo el cron `notificaciones:enviar` de aosc_front.
	NotificacionesHabilitadas bool
	FCMCredenciales           string
}

func Cargar() (*Config, error) {
	if err := cargarDotEnv(".env"); err != nil {
		return nil, err
	}

	perfil, err := instancia.Obtener(env("INSTANCIA", "aosc"))
	if err != nil {
		return nil, err
	}

	c := &Config{
		Instancia:                 perfil,
		HTTPAddr:                  env("HTTP_ADDR", ":8080"),
		DBHost:                    env("DB_HOST", "127.0.0.1"),
		DBPort:                    env("DB_PORT", "3306"),
		DBDatabase:                env("DB_DATABASE", "aosc"),
		DBUsername:                env("DB_USERNAME", "root"),
		DBPassword:                env("DB_PASSWORD", ""),
		JWTSecret:                 []byte(env("JWT_SECRET", "")),
		MailHost:                  env("MAIL_HOST", ""),
		MailPort:                  env("MAIL_PORT", "587"),
		MailUsername:              env("MAIL_USERNAME", ""),
		MailPassword:              env("MAIL_PASSWORD", ""),
		MailEncryption:            strings.ToLower(env("MAIL_ENCRYPTION", "tls")),
		MailFrom:                  env("MAIL_FROM_ADDRESS", ""),
		MailFromName:              env("MAIL_FROM_NAME", perfil.Nombre),
		ValidarCorreoURL:          strings.TrimRight(env("VALIDAR_CORREO_URL", "https://aosc.gob.ar/validacion"), "/"),
		ArchivosURL:               strings.TrimRight(env("ARCHIVOS_URL", ""), "/"),
		WeeHabilitado:             envBool("WEE_HABILITADO", true),
		NotificacionesHabilitadas: envBool("NOTIFICACIONES_HABILITADAS", false),
		FCMCredenciales:           env("FCM_CREDENTIALS", ""),
	}

	// Laravel usa "null" para "sin cifrado".
	if c.MailEncryption == "null" {
		c.MailEncryption = ""
	}

	if c.NotificacionesHabilitadas && !perfil.Push {
		return nil, fmt.Errorf("NOTIFICACIONES_HABILITADAS=true no es válido para la instancia %s: su base no tiene tablas de push", perfil.Nombre)
	}

	if len(c.JWTSecret) < 32 {
		return nil, fmt.Errorf("JWT_SECRET debe tener al menos 32 caracteres")
	}

	dias, err := strconv.Atoi(env("JWT_TTL_DIAS", "180"))
	if err != nil || dias <= 0 {
		return nil, fmt.Errorf("JWT_TTL_DIAS inválido")
	}
	c.JWTTTL = time.Duration(dias) * 24 * time.Hour

	return c, nil
}

// cargarDotEnv lee un archivo .env (si existe) sin pisar las variables que
// ya están en el entorno, así en Docker mandan las de env_file/environment.
func cargarDotEnv(ruta string) error {
	b, err := os.ReadFile(ruta)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("no se pudo leer %s: %w", ruta, err)
	}

	for _, linea := range strings.Split(string(b), "\n") {
		linea = strings.TrimSpace(strings.TrimSuffix(linea, "\r"))
		if linea == "" || strings.HasPrefix(linea, "#") {
			continue
		}
		linea = strings.TrimPrefix(linea, "export ")
		clave, valor, ok := strings.Cut(linea, "=")
		if !ok {
			continue
		}
		clave, valor = strings.TrimSpace(clave), strings.TrimSpace(valor)

		if len(valor) >= 2 && (valor[0] == '"' || valor[0] == '\'') && valor[len(valor)-1] == valor[0] {
			valor = valor[1 : len(valor)-1]
		} else if i := strings.Index(valor, " #"); i >= 0 {
			valor = strings.TrimSpace(valor[:i])
		}

		if _, existe := os.LookupEnv(clave); !existe {
			os.Setenv(clave, valor)
		}
	}
	return nil
}

func env(clave, def string) string {
	if v, ok := os.LookupEnv(clave); ok {
		return v
	}
	return def
}

func envBool(clave string, def bool) bool {
	v, ok := os.LookupEnv(clave)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
