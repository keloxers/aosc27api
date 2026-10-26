package pdf

import (
	"context"
	"os"
	"strconv"
	"testing"

	"aosc_api/internal/config"
	"aosc_api/internal/db"
	"aosc_api/internal/facturacion"
	"aosc_api/internal/wee"
)

// TestGenerar escribe el PDF de PDF_MOVIMIENTO en PDF_SALIDA para revisarlo
// a ojo contra el del portal. Necesita la base (DB_*) y opcionalmente
// ARCHIVOS_URL para el fondo. No llama a Wee.
func TestGenerar(t *testing.T) {
	id, _ := strconv.ParseInt(os.Getenv("PDF_MOVIMIENTO"), 10, 64)
	salida := os.Getenv("PDF_SALIDA")
	if id == 0 || salida == "" {
		t.Skip("PDF_MOVIMIENTO / PDF_SALIDA no definidos")
	}
	os.Setenv("JWT_SECRET", "x-x-x-x-x-x-x-x-x-x-x-x-x-x-x-x-x")
	cfg, err := config.Cargar()
	if err != nil {
		t.Fatal(err)
	}
	conexion, err := db.Abrir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conexion.Close()

	ctx := context.Background()
	m, err := facturacion.BuscarMovimiento(ctx, conexion, id)
	if err != nil || m == nil {
		t.Fatalf("movimiento %d: %v", id, err)
	}
	g := NuevoGenerador(conexion, wee.NuevoCobros(conexion, false), cfg.ArchivosURL)
	b, err := g.Generar(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(salida, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
