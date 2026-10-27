package facturacion

import (
	"database/sql"
	"math"
	"testing"
	"time"
)

// calcSinDB arma una Calculadora con todo precargado (sin base): mora 1% diario,
// IVA 21% para el titular 7 y sin conceptos.
func calcSinDB(m *Movimiento) *Calculadora {
	c := NuevaCalculadora(nil, nil)
	c.Ahora = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	p := 1.0
	c.mora, c.moraCargada = &p, true
	c.inmuebles[5] = &Inmueble{ID: 5, UsersID: sql.NullInt64{Int64: 7, Valid: true}}
	c.iva[7] = 21
	c.descuentos[5] = nil
	c.conceptos[m.ID] = nil
	c.detalles[m.ID] = nil
	return c
}

func movVencido() *Movimiento {
	return &Movimiento{
		ID:                  1,
		InmueblesID:         sql.NullInt64{Int64: 5, Valid: true},
		ImporteBruto:        sql.NullFloat64{Float64: 1000, Valid: true},
		ImporteApagar:       sql.NullFloat64{Float64: 1000, Valid: true}, // impago: apagar != pagado
		AplicaInteres:       sql.NullBool{Bool: true, Valid: true},
		Fecha1erVencimiento: sql.NullTime{Time: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), Valid: true},
	}
}

func TestImporteSinMora(t *testing.T) {
	m := movVencido()
	c := calcSinDB(m)

	final, err := c.ImporteFinal(m)
	if err != nil {
		t.Fatal(err)
	}
	sin, err := c.ImporteSinMora(m)
	if err != nil {
		t.Fatal(err)
	}
	// 10 días de atraso al 1%: final = (1000+100)*1.21; sin mora = 1000*1.21.
	if math.Abs(final-1331) > 0.001 || math.Abs(sin-1210) > 0.001 {
		t.Fatalf("final=%.2f sin=%.2f", final, sin)
	}

	// En término no hay mora: los dos importes coinciden.
	m.Fecha1erVencimiento.Time = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	final, _ = c.ImporteFinal(m)
	sin, _ = c.ImporteSinMora(m)
	if final != sin {
		t.Fatalf("en término: final=%.2f sin=%.2f", final, sin)
	}
}

func TestImporteSinMoraPagado(t *testing.T) {
	m := movVencido()
	m.ImporteApagar = sql.NullFloat64{Float64: 1331, Valid: true}
	m.ImportePagado = sql.NullFloat64{Float64: 1331, Valid: true}
	c := calcSinDB(m)
	final, _ := c.ImporteFinal(m)
	sin, _ := c.ImporteSinMora(m)
	if final != 1331 || sin != 1331 {
		t.Fatalf("pagado: final=%.2f sin=%.2f", final, sin)
	}
}
