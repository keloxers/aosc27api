// Package api expone /api/v1 con el mismo contrato que la API Sanctum de
// aosc_front (routes/api.php), para que aosc_app solo cambie la base URL.
package api

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"aosc_api/internal/auth"
	"aosc_api/internal/mail"
	"aosc_api/internal/pdf"
	"aosc_api/internal/wee"
)

type Servidor struct {
	db        *sql.DB
	auth      *auth.Servicio
	limitador *auth.Limitador
	mailer    *mail.Mailer
	cobros    *wee.Cobros
	pdf       *pdf.Generador
}

func NuevoServidor(db *sql.DB, a *auth.Servicio, m *mail.Mailer, c *wee.Cobros, p *pdf.Generador) *Servidor {
	return &Servidor{
		db:        db,
		auth:      a,
		limitador: auth.NuevoLimitador(5, time.Minute),
		mailer:    m,
		cobros:    c,
		pdf:       p,
	}
}

func (s *Servidor) Limitador() *auth.Limitador { return s.limitador }

func (s *Servidor) Handler() http.Handler {
	mux := http.NewServeMux()
	a := s.requiereAuth

	mux.HandleFunc("GET /up", func(w http.ResponseWriter, r *http.Request) {
		if err := s.db.PingContext(r.Context()); err != nil {
			mensaje(w, http.StatusServiceUnavailable, "sin base de datos")
			return
		}
		responderJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /api/v1/register", s.register)
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("POST /api/v1/logout", a(s.logout))
	mux.HandleFunc("GET /api/v1/me", a(s.me))

	mux.HandleFunc("GET /api/v1/medidores", a(s.medidoresIndex))
	mux.HandleFunc("POST /api/v1/medidores", a(s.medidoresStore))
	mux.HandleFunc("DELETE /api/v1/medidores/{inmueble}", a(s.medidoresDestroy))

	mux.HandleFunc("GET /api/v1/medidores/{inmueble}/facturas", a(s.facturasIndex))
	mux.HandleFunc("GET /api/v1/medidores/{inmueble}/facturas/{movimiento}", a(s.facturasShow))
	mux.HandleFunc("GET /api/v1/medidores/{inmueble}/facturas/{movimiento}/pdf", a(s.facturasPdf))
	mux.HandleFunc("GET /api/v1/medidores/{inmueble}/consumos", a(s.consumos))
	mux.HandleFunc("GET /api/v1/medidores/{inmueble}/pagos", a(s.pagos))
	mux.HandleFunc("POST /api/v1/checkout", a(s.checkout))
	mux.HandleFunc("GET /api/v1/checkout/{ref}/status", a(s.checkoutStatus))

	mux.HandleFunc("POST /api/v1/dispositivos", a(s.dispositivosStore))
	mux.HandleFunc("DELETE /api/v1/dispositivos", a(s.dispositivosDestroy))

	return recuperar(registrar(mux))
}

func registrar(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inicio := time.Now()
		rr := &respuestaRegistrada{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rr, r)
		if r.URL.Path == "/up" {
			return
		}
		slog.Info("request", "metodo", r.Method, "ruta", r.URL.Path, "status", rr.status,
			"ms", time.Since(inicio).Milliseconds(), "ip", ipCliente(r))
	})
}
