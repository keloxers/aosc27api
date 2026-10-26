package wee

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestQRFacturaRechazaRespuestaSinURLQR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"uuid":"u1","checkout_url":"https://x/c"}]}`))
	}))
	defer srv.Close()

	_, _, _, err := NuevoCliente().QRFactura(context.Background(), srv.URL, "k", Pedido{})
	if err == nil {
		t.Fatal("esperaba error por falta de url_qr")
	}
}

func TestQRFacturaOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"uuid":"u1","url_qr":"https://x/qr","checkout_url":"https://x/c"}]}`))
	}))
	defer srv.Close()

	uuid, qr, co, err := NuevoCliente().QRFactura(context.Background(), srv.URL, "k", Pedido{})
	if err != nil || uuid != "u1" || qr != "https://x/qr" || co != "https://x/c" {
		t.Fatalf("resultado inesperado: %q %q %q %v", uuid, qr, co, err)
	}
}

func TestNoSigueRedirecciones(t *testing.T) {
	var destino atomic.Int32
	otro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destino.Add(1)
	}))
	defer otro.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otro.URL, http.StatusFound)
	}))
	defer srv.Close()

	_, _, err := NuevoCliente().Checkout(context.Background(), srv.URL, "k", Pedido{})
	if err == nil {
		t.Fatal("esperaba error ante un 3xx")
	}
	if destino.Load() != 0 {
		t.Fatal("se siguió la redirección (se filtraría el apiKey)")
	}
}

func TestErrorTruncaBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer srv.Close()

	_, _, err := NuevoCliente().Checkout(context.Background(), srv.URL, "k", Pedido{})
	if err == nil {
		t.Fatal("esperaba error")
	}
	if n := len(err.Error()); n > 350 {
		t.Fatalf("el error no se truncó: %d bytes", n)
	}
}

func TestResultado(t *testing.T) {
	ns := func(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
	casos := []struct {
		estado, transaccion, want string
	}{
		{"pagando", "completado", "pagado"},
		{"DUPLICADO", "pendiente", "duplicado"},
		{"error", "rechazado", "fallido"},
		{"pagando", "pendiente", "pendiente"},
		{"", "", "pendiente"},
	}
	for _, c := range casos {
		if got := Resultado(ns(c.estado), ns(c.transaccion)); got != c.want {
			t.Errorf("Resultado(%q,%q) = %q, quería %q", c.estado, c.transaccion, got, c.want)
		}
	}
}
