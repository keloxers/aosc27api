package db

import (
	"testing"
	"time"
)

// 22:30 en Argentina es 01:30 UTC del día siguiente: "hoy" debe ser el día AR.
func TestZonaDiaArgentina(t *testing.T) {
	utc := time.Date(2026, 10, 28, 1, 30, 0, 0, time.UTC)
	ar := utc.In(Zona)
	if got := ar.Format("2006-01-02 15:04:05"); got != "2026-10-27 22:30:00" {
		t.Fatalf("hora AR = %s", got)
	}
}

func TestAhoraYHoyEnZonaAR(t *testing.T) {
	if AhoraAR().Location() != Zona {
		t.Fatal("AhoraAR no está en Zona")
	}
	if HoyAR() != Ahora()[:10] {
		t.Fatalf("HoyAR %s != Ahora %s", HoyAR(), Ahora())
	}
}
