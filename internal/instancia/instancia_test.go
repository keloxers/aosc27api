package instancia

import "testing"

func TestObtener(t *testing.T) {
	casos := []struct {
		nombre  string
		err     bool
		titulo  string
		push    bool
		concept float64
	}{
		{"aosc", false, "AOSC", true, 166},
		{"covesa", false, "COVESA Ituzaingó", false, 173},
		{"COVESA", false, "COVESA Ituzaingó", false, 173},
		{"otra", true, "", false, 0},
		{"", true, "", false, 0},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			pf, err := Obtener(c.nombre)
			if c.err {
				if err == nil {
					t.Fatal("se esperaba error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if pf.Nombre != c.titulo || pf.Push != c.push || pf.Factura.ConceptosY != c.concept {
				t.Fatalf("perfil inesperado: %+v", pf)
			}
		})
	}
}
