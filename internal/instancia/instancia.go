// Package instancia define los valores propios de cada instancia que sirve
// esta API (AOSC o COVESA), elegida con la variable INSTANCIA.
package instancia

import (
	"fmt"
	"strings"
)

// Pos es una posición en mm sobre la plantilla de la factura.
type Pos struct{ X, Y float64 }

// Factura son las posiciones de cada dato en el PDF de la factura.
type Factura struct {
	Cuenta               Pos
	Usuario              Pos
	Iva                  Pos
	Cuit                 Pos
	Direccion            Pos
	Localidad            Pos
	Categoria            Pos
	Medidor              Pos
	FechaLecturaAnterior Pos
	LecturaAnterior      Pos
	FechaUltimaLectura   Pos
	UltimaLectura        Pos
	ConsumoMensual       Pos
	ConsumoPromedio      Pos
	ValorM3              Pos
	NumFactura           Pos
	FechaEmision         Pos
	Periodo              Pos
	// ConceptosY es la Y inicial de la lista de conceptos.
	ConceptosY float64
}

// Perfil agrupa lo que cambia entre instancias.
type Perfil struct {
	Nombre string
	// Push indica si la base tiene las tablas de dispositivos y notificaciones.
	Push           bool
	WeeDescripcion string
	Factura        Factura
}

func p(x, y float64) Pos { return Pos{x, y} }

var perfiles = map[string]Perfil{
	"aosc": {
		Nombre:         "AOSC",
		Push:           true,
		WeeDescripcion: "Dist. de agua potable y cloaca",
		Factura: Factura{
			Cuenta: p(120, 54.5), Usuario: p(117, 61), Iva: p(134, 68), Cuit: p(117, 80),
			Direccion: p(120, 85.7), Localidad: p(120, 90), Categoria: p(141, 94.5),
			Medidor:              p(115, 104.5),
			FechaLecturaAnterior: p(135, 108.5), LecturaAnterior: p(126, 113),
			FechaUltimaLectura: p(133, 117), UltimaLectura: p(124, 121.5),
			ConsumoMensual: p(137, 125.5), ConsumoPromedio: p(147, 129.5), ValorM3: p(158, 134),
			NumFactura: p(122, 146), FechaEmision: p(129, 151), Periodo: p(115, 156),
			ConceptosY: 166,
		},
	},
	"covesa": {
		Nombre:         "COVESA Ituzaingó",
		Push:           false,
		WeeDescripcion: "Dist. de agua potable y cloaca",
		Factura: Factura{
			Cuenta: p(120.5, 54.5), Usuario: p(117, 61.6), Iva: p(135, 67.8), Cuit: p(116, 80.7),
			Direccion: p(120, 85.5), Localidad: p(121, 90.6), Categoria: p(141.5, 100.2),
			Medidor:              p(114.5, 111.6),
			FechaLecturaAnterior: p(134.5, 116.1), LecturaAnterior: p(126, 120.4),
			FechaUltimaLectura: p(132.5, 124.4), UltimaLectura: p(123, 128.4),
			ConsumoMensual: p(137, 133.1), ConsumoPromedio: p(147.5, 137.2), ValorM3: p(157, 141.3),
			NumFactura: p(123, 153.4), FechaEmision: p(129, 157.8), Periodo: p(113, 162.8),
			ConceptosY: 173,
		},
	},
}

// Obtener devuelve el perfil de la instancia o falla si no existe.
func Obtener(nombre string) (*Perfil, error) {
	pf, ok := perfiles[strings.ToLower(strings.TrimSpace(nombre))]
	if !ok {
		return nil, fmt.Errorf("INSTANCIA inválida %q (válidas: aosc, covesa)", nombre)
	}
	return &pf, nil
}
