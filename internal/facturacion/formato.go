package facturacion

import (
	"strconv"
	"strings"
)

// NumberFormat replica number_format() de PHP.
func NumberFormat(x float64, decimales int, sepDecimal, sepMiles string) string {
	x = RedondearPHP(x, decimales)
	negativo := x < 0
	if negativo {
		x = -x
	}

	s := strconv.FormatFloat(x, 'f', decimales, 64)
	entera, frac, _ := strings.Cut(s, ".")

	var b strings.Builder
	for i, r := range entera {
		if i > 0 && (len(entera)-i)%3 == 0 {
			b.WriteString(sepMiles)
		}
		b.WriteRune(r)
	}
	if decimales > 0 {
		b.WriteString(sepDecimal)
		b.WriteString(frac)
	}

	if negativo && x != 0 {
		return "-" + b.String()
	}
	return b.String()
}

// Moneda formatea como en las vistas: $1.234,56.
func Moneda(x float64) string {
	return "$" + NumberFormat(x, 2, ",", ".")
}

// RellenarIzq equivale a str_pad($v, $n, $relleno, STR_PAD_LEFT).
func RellenarIzq(v string, n int, relleno string) string {
	if len(v) >= n {
		return v
	}
	return strings.Repeat(relleno, n-len(v)) + v
}
