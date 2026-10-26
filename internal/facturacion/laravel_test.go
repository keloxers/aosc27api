package facturacion

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"

	"aosc_api/internal/config"
	"aosc_api/internal/db"
)

// TestLaravel compara los cálculos con los de aosc_front sobre la misma base
// (ver herramientas/referencia_laravel.php). Se saltea si no se indica
// LARAVEL_REFERENCIA.
func TestLaravel(t *testing.T) {
	ruta := os.Getenv("LARAVEL_REFERENCIA")
	if ruta == "" {
		t.Skip("LARAVEL_REFERENCIA no definido")
	}
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Movimientos []struct {
			ID             int64    `json:"id"`
			ImporteFinal   float64  `json:"importe_final"`
			Intereses      float64  `json:"intereses"`
			Descuento      float64  `json:"descuento"`
			Descuentos     []string `json:"descuentos"`
			Recargo        float64  `json:"recargo"`
			Recargos       []string `json:"recargos"`
			ImporteIva     float64  `json:"importe_iva"`
			ConceptosTotal float64  `json:"conceptos_total"`
			Estado         string   `json:"estado"`
		} `json:"movimientos"`
		Inmuebles []struct {
			ID             int64   `json:"id"`
			TotalPendiente float64 `json:"total_pendiente"`
			Cantidad       int     `json:"cantidad_pendientes"`
			Estado         string  `json:"estado"`
		} `json:"inmuebles"`
	}
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
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
	calc := NuevaCalculadora(ctx, conexion)
	igual := func(a, b float64) bool { return math.Abs(a-b) < 0.005 }
	fallas := 0
	fallo := func(f string, args ...any) {
		fallas++
		if fallas <= 40 {
			t.Errorf(f, args...)
		}
	}

	for _, r := range ref.Movimientos {
		m, err := BuscarMovimiento(ctx, conexion, r.ID)
		if err != nil || m == nil {
			t.Fatalf("movimiento %d: %v", r.ID, err)
		}
		final, _ := calc.ImporteFinal(m)
		intereses, _ := calc.Intereses(m)
		descuento, nombresD, _ := calc.Descuentos(m)
		recargo, nombresR, _ := calc.Recargos(m)
		iva, _ := calc.ImporteIva(m)
		conceptos, _ := calc.ConceptosTotal(m)

		if !igual(final, r.ImporteFinal) {
			fallo("mov %d importe_final go=%.4f php=%.4f", r.ID, final, r.ImporteFinal)
		}
		if !igual(intereses, r.Intereses) {
			fallo("mov %d intereses go=%.4f php=%.4f", r.ID, intereses, r.Intereses)
		}
		if !igual(descuento, r.Descuento) || !reflect.DeepEqual(nombresD, append([]string{}, r.Descuentos...)) {
			fallo("mov %d descuento go=%.4f %v php=%.4f %v", r.ID, descuento, nombresD, r.Descuento, r.Descuentos)
		}
		if !igual(recargo, r.Recargo) || !reflect.DeepEqual(nombresR, append([]string{}, r.Recargos...)) {
			fallo("mov %d recargo go=%.4f %v php=%.4f %v", r.ID, recargo, nombresR, r.Recargo, r.Recargos)
		}
		if !igual(iva, r.ImporteIva) {
			fallo("mov %d iva go=%.4f php=%.4f", r.ID, iva, r.ImporteIva)
		}
		if !igual(conceptos, r.ConceptosTotal) {
			fallo("mov %d conceptos go=%.4f php=%.4f", r.ID, conceptos, r.ConceptosTotal)
		}
		if e := calc.EstadoMovimiento(m); e != r.Estado {
			fallo("mov %d estado go=%s php=%s", r.ID, e, r.Estado)
		}
	}

	for _, r := range ref.Inmuebles {
		pendientes, err := Pendientes(ctx, conexion, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		total := 0.0
		for _, m := range pendientes {
			f, _ := calc.ImporteFinal(m)
			total += f
		}
		if !igual(total, r.TotalPendiente) || len(pendientes) != r.Cantidad {
			fallo("inmueble %d total go=%.4f (%d) php=%.4f (%d)", r.ID, total, len(pendientes), r.TotalPendiente, r.Cantidad)
		}
		if e := calc.EstadoInmueble(pendientes); e != r.Estado {
			fallo("inmueble %d estado go=%s php=%s", r.ID, e, r.Estado)
		}
	}

	t.Logf("comparados %d movimientos y %d inmuebles, %d diferencias", len(ref.Movimientos), len(ref.Inmuebles), fallas)
}
