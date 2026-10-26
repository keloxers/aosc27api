package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"aosc_api/internal/auth"
)

type ctxClave int

const ctxSesion ctxClave = iota

func sesionDe(r *http.Request) *auth.Sesion {
	s, _ := r.Context().Value(ctxSesion).(*auth.Sesion)
	return s
}

func responderJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func mensaje(w http.ResponseWriter, status int, texto string) {
	responderJSON(w, status, map[string]string{"message": texto})
}

func sinContenido(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func errorInterno(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("error interno", "metodo", r.Method, "ruta", r.URL.Path, "error", err)
	mensaje(w, http.StatusInternalServerError, "Server Error")
}

// Errores de validación con el formato de Laravel: {message, errors}.
type errores map[string][]string

func (e errores) agregar(campo, texto string) {
	e[campo] = append(e[campo], texto)
}

func (e errores) responder(w http.ResponseWriter) bool {
	if len(e) == 0 {
		return false
	}
	responderJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"message": "Los datos ingresados no son válidos.",
		"errors":  e,
	})
	return true
}

// entrada es el cuerpo del request (JSON o formulario), como $request->all().
type entrada map[string]any

func leerEntrada(r *http.Request) (entrada, error) {
	cuerpo, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	e := entrada{}
	if len(bytes.TrimSpace(cuerpo)) == 0 {
		return e, nil
	}

	if strings.Contains(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		valores, err := url.ParseQuery(string(cuerpo))
		if err != nil {
			return nil, err
		}
		for k, v := range valores {
			if strings.HasSuffix(k, "[]") {
				lista := make([]any, len(v))
				for i, s := range v {
					lista[i] = s
				}
				e[strings.TrimSuffix(k, "[]")] = lista
			} else {
				e[k] = v[len(v)-1]
			}
		}
		return e, nil
	}

	dec := json.NewDecoder(bytes.NewReader(cuerpo))
	dec.UseNumber()
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("JSON inválido: %w", err)
	}
	return e, nil
}

// texto devuelve el valor como string si es escalar (los números JSON se
// aceptan como string, igual que PHP al validar `numeric`/`digits`).
func (e entrada) texto(campo string) (string, bool) {
	switch v := e[campo].(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		if v {
			return "1", true
		}
		return "", true
	}
	return "", false
}

// vacio replica la regla `required` de Laravel.
func (e entrada) vacio(campo string) bool {
	v, ok := e[campo]
	if !ok || v == nil {
		return true
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

var (
	reEmail   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	reDigitos = regexp.MustCompile(`^[0-9]+$`)
)

func esNumerico(s string) bool {
	_, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return err == nil
}

// ipCliente: primera IP de X-Forwarded-For (detrás del proxy) o la remota.
func ipCliente(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func idDeRuta(r *http.Request, nombre string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(nombre), 10, 64)
	return id, err == nil && id > 0
}

// requiereAuth valida el Bearer token y deja la sesión en el contexto.
func (s *Servidor) requiereAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || strings.TrimSpace(token) == "" {
			mensaje(w, http.StatusUnauthorized, "Unauthenticated.")
			return
		}
		sesion, err := s.auth.Validar(r.Context(), strings.TrimSpace(token))
		if errors.Is(err, auth.ErrTokenInvalido) {
			mensaje(w, http.StatusUnauthorized, "Unauthenticated.")
			return
		}
		if err != nil {
			errorInterno(w, r, err)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxSesion, sesion)))
	}
}

// respuestaRegistrada guarda el status para el log de requests.
type respuestaRegistrada struct {
	http.ResponseWriter
	status int
}

func (r *respuestaRegistrada) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func recuperar(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				errorInterno(w, r, fmt.Errorf("panic: %v", p))
			}
		}()
		h.ServeHTTP(w, r)
	})
}
