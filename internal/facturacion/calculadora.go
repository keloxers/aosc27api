package facturacion

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	bd "aosc_api/internal/db"
)

// Calculadora evalúa los importes de los movimientos con un "ahora" fijo y
// cachea lo que se consulta varias veces (inmuebles, descuentos, interés de
// mora, IVA del titular...). Se crea una por request o por corrida.
type Calculadora struct {
	db    *sql.DB
	ctx   context.Context
	Ahora time.Time // hora de Argentina, como Carbon::now() en Laravel

	inmuebles   map[int64]*Inmueble
	descuentos  map[int64][]descuentoInmueble
	exentos     map[int64]bool
	tarifas     map[int64]*tarifa
	iva         map[int64]float64
	conceptos   map[int64][]Concepto
	detalles    map[int64]*detallePago
	mora        *float64
	moraCargada bool
}

func NuevaCalculadora(ctx context.Context, db *sql.DB) *Calculadora {
	return &Calculadora{
		db:         db,
		ctx:        ctx,
		Ahora:      bd.AhoraAR(),
		inmuebles:  map[int64]*Inmueble{},
		descuentos: map[int64][]descuentoInmueble{},
		exentos:    map[int64]bool{},
		tarifas:    map[int64]*tarifa{},
		iva:        map[int64]float64{},
		conceptos:  map[int64][]Concepto{},
		detalles:   map[int64]*detallePago{},
	}
}

type descuentoInmueble struct {
	Tipo         string // descuento | recargo
	Porcentaje   float64
	DescontarVto bool
	Nombre       string
	Desde        sql.NullTime
	Hasta        sql.NullTime
}

type tarifa struct {
	DescuentoPorcentaje float64
	DescuentoFijo       float64
}

type Concepto struct {
	Nombre        string
	Importe       float64
	ImportePagado sql.NullFloat64
}

type detallePago struct {
	ImportePagado float64
	ReciboCerrado bool
}

// Hoy en formato Y-m-d (date('Y-m-d') en PHP).
func (c *Calculadora) Hoy() string {
	return c.Ahora.Format("2006-01-02")
}

func (c *Calculadora) inicioDelDia() time.Time {
	y, mo, d := c.Ahora.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, bd.Zona)
}

// Inmueble del movimiento (cacheado). Nil si el movimiento no tiene.
func (c *Calculadora) Inmueble(id sql.NullInt64) (*Inmueble, error) {
	if !id.Valid {
		return nil, nil
	}
	if i, ok := c.inmuebles[id.Int64]; ok {
		return i, nil
	}
	i, err := BuscarInmueble(c.ctx, c.db, id.Int64)
	if err != nil {
		return nil, err
	}
	c.inmuebles[id.Int64] = i
	return i, nil
}

// Exento: el inmueble tiene una exención cargada cuyo nombre no es "no".
func (c *Calculadora) Exento(inm *Inmueble) (bool, error) {
	if inm == nil || !inm.ExencionsID.Valid {
		return false, nil
	}
	if e, ok := c.exentos[inm.ID]; ok {
		return e, nil
	}
	var nombre sql.NullString
	err := c.db.QueryRowContext(c.ctx, "SELECT exencion FROM exencions WHERE id = ?", inm.ExencionsID.Int64).Scan(&nombre)
	exento := false
	if err == nil {
		exento = strings.ToLower(nombre.String) != "no"
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	c.exentos[inm.ID] = exento
	return exento, nil
}

func (c *Calculadora) descuentosDe(inm *Inmueble) ([]descuentoInmueble, error) {
	if inm == nil {
		return nil, nil
	}
	if d, ok := c.descuentos[inm.ID]; ok {
		return d, nil
	}
	rows, err := c.db.QueryContext(c.ctx, `
		SELECT d.tipo, d.porcentaje, d.descontar_vto, d.descuento, d.desde, d.hasta
		FROM inmuebles_descuentos idd
		JOIN descuentos d ON d.id = idd.descuentos_id
		WHERE idd.inmuebles_id = ?
		ORDER BY idd.id`, inm.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lista []descuentoInmueble
	for rows.Next() {
		var d descuentoInmueble
		var nombre sql.NullString
		if err := rows.Scan(&d.Tipo, &d.Porcentaje, &d.DescontarVto, &nombre, &d.Desde, &d.Hasta); err != nil {
			return nil, err
		}
		d.Nombre = nombre.String
		lista = append(lista, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.descuentos[inm.ID] = lista
	return lista, nil
}

// enRango replica Carbon::between() (inclusivo, intercambia los extremos si
// vienen al revés). Una fecha nula vale "ahora", como Carbon::parse(null).
func (c *Calculadora) enRango(fecha, desde, hasta sql.NullTime) bool {
	f, d, h := c.oAhora(fecha), c.oAhora(desde), c.oAhora(hasta)
	if d.After(h) {
		d, h = h, d
	}
	return !f.Before(d) && !f.After(h)
}

func (c *Calculadora) oAhora(t sql.NullTime) time.Time {
	if t.Valid {
		return t.Time
	}
	return c.Ahora
}

func (c *Calculadora) tarifaEspecial(ownerID int64) (*tarifa, error) {
	if t, ok := c.tarifas[ownerID]; ok {
		return t, nil
	}
	var t tarifa
	hoy := c.Hoy()
	err := c.db.QueryRowContext(c.ctx, `
		SELECT t.descuento_porcentaje, t.descuento_fijo
		FROM tarifasespecials te
		JOIN tarifas t ON t.id = te.tarifa_id
		WHERE DATE(te.vigencia_desde) <= ? AND DATE(te.vigencia_hasta) >= ? AND te.users_id = ?
		ORDER BY te.id LIMIT 1`, hoy, hoy, ownerID).Scan(&t.DescuentoPorcentaje, &t.DescuentoFijo)
	if errors.Is(err, sql.ErrNoRows) {
		c.tarifas[ownerID] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.tarifas[ownerID] = &t
	return &t, nil
}

// Descuentos devuelve el importe a descontar y los nombres de los
// descuentos con vigencia (Movimiento::descuentos()).
func (c *Calculadora) Descuentos(m *Movimiento) (float64, []string, error) {
	nombres := []string{}
	if !m.AplicaDescuento {
		return 0, nombres, nil
	}

	inm, err := c.Inmueble(m.InmueblesID)
	if err != nil {
		return 0, nil, err
	}
	lista, err := c.descuentosDe(inm)
	if err != nil {
		return 0, nil, err
	}

	porcentaje, fijo := 0.0, 0.0
	// Comparación de strings Y-m-d como en PHP; sin vencimiento no descuenta.
	enTermino := m.Fecha1erVencimiento.Valid && c.Hoy() <= m.Fecha1erVencimiento.Time.Format("2006-01-02")

	for _, d := range lista {
		if !(d.DescontarVto || enTermino) || d.Tipo != "descuento" {
			continue
		}
		if !d.Desde.Valid && !d.Hasta.Valid {
			porcentaje += d.Porcentaje
		} else if c.enRango(m.CreatedAt, d.Desde, d.Hasta) {
			nombres = append(nombres, d.Nombre)
			porcentaje += d.Porcentaje
		}
	}

	if inm != nil && inm.UsersID.Valid {
		t, err := c.tarifaEspecial(inm.UsersID.Int64)
		if err != nil {
			return 0, nil, err
		}
		if t != nil {
			porcentaje += t.DescuentoPorcentaje
			fijo += t.DescuentoFijo
		}
	}

	exento, err := c.Exento(inm)
	if err != nil {
		return 0, nil, err
	}
	if exento {
		porcentaje = 100
	}

	return m.ImporteBruto.Float64*(porcentaje/100) - fijo, nombres, nil
}

// Recargos (Movimiento::recargos()): descuentos del inmueble de tipo recargo.
func (c *Calculadora) Recargos(m *Movimiento) (float64, []string, error) {
	nombres := []string{}
	inm, err := c.Inmueble(m.InmueblesID)
	if err != nil {
		return 0, nil, err
	}
	lista, err := c.descuentosDe(inm)
	if err != nil {
		return 0, nil, err
	}

	recargo := 0.0
	for _, d := range lista {
		if d.Tipo != "recargo" {
			continue
		}
		if !d.Desde.Valid && !d.Hasta.Valid {
			recargo += m.ImporteBruto.Float64 * (d.Porcentaje / 100)
		} else if c.enRango(m.CreatedAt, d.Desde, d.Hasta) {
			nombres = append(nombres, d.Nombre)
			recargo += m.ImporteBruto.Float64 * (d.Porcentaje / 100)
		}
	}
	return recargo, nombres, nil
}

func (c *Calculadora) porcentajeMora() (float64, error) {
	if c.moraCargada {
		if c.mora == nil {
			return 0, nil
		}
		return *c.mora, nil
	}
	var p float64
	err := c.db.QueryRowContext(c.ctx, `
		SELECT porcentaje FROM intereses
		WHERE intereses = 'mora' AND vigencia <= ?
		ORDER BY vigencia DESC LIMIT 1`, c.Hoy()).Scan(&p)
	c.moraCargada = true
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		c.moraCargada = false
		return 0, err
	}
	c.mora = &p
	return p, nil
}

// diasEntre replica Carbon::diffInDays(): días completos, valor absoluto.
func diasEntre(a, b time.Time) float64 {
	return math.Floor(math.Abs(b.Sub(a).Hours()) / 24)
}

// Intereses por mora desde el primer vencimiento (Movimiento::intereses()).
func (c *Calculadora) Intereses(m *Movimiento) (float64, error) {
	if !m.AplicaInteres.Valid || !m.AplicaInteres.Bool {
		return 0, nil
	}
	inm, err := c.Inmueble(m.InmueblesID)
	if err != nil {
		return 0, err
	}
	exento, err := c.Exento(inm)
	if err != nil {
		return 0, err
	}
	if exento {
		return 0, nil
	}

	// Sin vencimiento Carbon::parse(null) es "ahora": cero días.
	if !m.Fecha1erVencimiento.Valid {
		return 0, nil
	}
	vencimiento := m.Fecha1erVencimiento.Time

	if !c.Ahora.After(vencimiento) {
		return 0, nil
	}

	dias := diasEntre(vencimiento, c.Ahora)
	if m.FechaPago.Valid {
		if m.FechaPago.Time.After(vencimiento) {
			dias = diasEntre(vencimiento, m.FechaPago.Time)
		} else {
			dias = 0
		}
	}

	porcentaje, err := c.porcentajeMora()
	if err != nil {
		return 0, err
	}

	return (m.ImporteBruto.Float64 * (porcentaje / 100)) * dias, nil
}

// Conceptos fijos asociados al movimiento.
func (c *Calculadora) Conceptos(m *Movimiento) ([]Concepto, error) {
	if l, ok := c.conceptos[m.ID]; ok {
		return l, nil
	}
	rows, err := c.db.QueryContext(c.ctx, `
		SELECT cf.conceptosfijo, cf.importe, mcf.importe_pagado
		FROM movimientosconceptosfijos mcf
		JOIN conceptosfijos cf ON cf.id = mcf.conceptosfijos_id
		WHERE mcf.movimientos_id = ?
		ORDER BY mcf.id`, m.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	lista := []Concepto{}
	for rows.Next() {
		var cp Concepto
		if err := rows.Scan(&cp.Nombre, &cp.Importe, &cp.ImportePagado); err != nil {
			return nil, err
		}
		lista = append(lista, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.conceptos[m.ID] = lista
	return lista, nil
}

// ConceptosTotal suma los conceptos todavía impagos.
func (c *Calculadora) ConceptosTotal(m *Movimiento) (float64, error) {
	lista, err := c.Conceptos(m)
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, cp := range lista {
		if !cp.ImportePagado.Valid || cp.ImportePagado.Float64 == 0 {
			total += cp.Importe
		}
	}
	return total, nil
}

// detallePagado: primer detalle de recibo con importe pagado del movimiento.
func (c *Calculadora) detallePagado(m *Movimiento) (*detallePago, error) {
	if d, ok := c.detalles[m.ID]; ok {
		return d, nil
	}
	var d detallePago
	var estado sql.NullString
	err := c.db.QueryRowContext(c.ctx, `
		SELECT crd.importe_pagado, cr.estado
		FROM cajasrecibosdetalles crd
		LEFT JOIN cajasrecibos cr ON cr.id = crd.cajasrecibos_id
		WHERE crd.movimientos_id = ? AND crd.importe_pagado > 0
		ORDER BY crd.id LIMIT 1`, m.ID).Scan(&d.ImportePagado, &estado)
	if errors.Is(err, sql.ErrNoRows) {
		c.detalles[m.ID] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.ReciboCerrado = estado.String == "cerrada"
	c.detalles[m.ID] = &d
	return &d, nil
}

// igualesPHP replica `importe_apagar == importe_pagado` entre dos decimales
// que pueden ser null.
func igualesPHP(a, b sql.NullFloat64) bool {
	if !a.Valid || !b.Valid {
		return a.Valid == b.Valid
	}
	return a.Float64 == b.Float64
}

// montoBase: lo pagado si la factura ya se cobró (en ese caso `cobrado` es
// true y el importe es definitivo), o el importe sin IVA si no.
func (c *Calculadora) montoBase(m *Movimiento) (monto float64, cobrado bool, err error) {
	return c.montoBaseMora(m, true)
}

// montoBaseMora es montoBase con la mora opcional (conMora=false: intereses 0).
func (c *Calculadora) montoBaseMora(m *Movimiento, conMora bool) (monto float64, cobrado bool, err error) {
	if igualesPHP(m.ImporteApagar, m.ImportePagado) {
		return m.ImportePagado.Float64, true, nil
	}

	d, err := c.detallePagado(m)
	if err != nil {
		return 0, false, err
	}
	if d != nil && d.ReciboCerrado {
		return d.ImportePagado, true, nil
	}

	intereses := 0.0
	if conMora {
		if intereses, err = c.Intereses(m); err != nil {
			return 0, false, err
		}
	}
	recargos, _, err := c.Recargos(m)
	if err != nil {
		return 0, false, err
	}
	conceptos, err := c.ConceptosTotal(m)
	if err != nil {
		return 0, false, err
	}
	descuentos, _, err := c.Descuentos(m)
	if err != nil {
		return 0, false, err
	}

	return m.ImporteBruto.Float64 + intereses + recargos + conceptos - descuentos, false, nil
}

// PorcentajeIva del titular del inmueble (alícuota de su tipo de IVA). Si el
// titular no tiene tipo de IVA válido se le asigna el 1 (Consumidor Final,
// 21%), igual que hace Movimiento::importeIva() en Laravel.
func (c *Calculadora) PorcentajeIva(inm *Inmueble) (float64, error) {
	if inm == nil || !inm.UsersID.Valid {
		return 0, nil
	}
	owner := inm.UsersID.Int64
	if p, ok := c.iva[owner]; ok {
		return p, nil
	}

	var desc sql.NullString
	err := c.db.QueryRowContext(c.ctx, "SELECT a.`desc` FROM users u"+
		" JOIN ivastipos it ON it.id = u.ivastipos_id"+
		" JOIN `alícuotas_disponibles` a ON a.id = it.alicuotas_id"+
		" WHERE u.id = ?", owner).Scan(&desc)

	if errors.Is(err, sql.ErrNoRows) {
		var existe int
		if err := c.db.QueryRowContext(c.ctx, "SELECT COUNT(*) FROM users WHERE id = ?", owner).Scan(&existe); err != nil {
			return 0, err
		}
		if existe == 0 {
			c.iva[owner] = 0
			return 0, nil
		}
		if _, err := c.db.ExecContext(c.ctx, "UPDATE users SET ivastipos_id = 1, updated_at = ? WHERE id = ?",
			c.Ahora.Format("2006-01-02 15:04:05"), owner); err != nil {
			return 0, err
		}
		c.iva[owner] = 21
		return 21, nil
	}
	if err != nil {
		return 0, err
	}

	p, _ := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(desc.String, "%", "")), 64)
	c.iva[owner] = p
	return p, nil
}

// ImporteIva (Movimiento::importeIva()).
func (c *Calculadora) ImporteIva(m *Movimiento) (float64, error) {
	return c.importeIvaMora(m, true)
}

func (c *Calculadora) importeIvaMora(m *Movimiento, conMora bool) (float64, error) {
	total, _, err := c.montoBaseMora(m, conMora)
	if err != nil {
		return 0, err
	}
	inm, err := c.Inmueble(m.InmueblesID)
	if err != nil {
		return 0, err
	}
	p, err := c.PorcentajeIva(inm)
	if err != nil {
		return 0, err
	}
	return p * total / 100, nil
}

// ImporteFinal: lo que hay que pagar hoy (Movimiento::importeFinal()).
func (c *Calculadora) ImporteFinal(m *Movimiento) (float64, error) {
	return c.importeFinalMora(m, true)
}

// ImporteSinMora: ImporteFinal como si Intereses fuera 0 (IVA coherente). Es el
// importe del QR de la factura, que no cambia con los días de atraso.
func (c *Calculadora) ImporteSinMora(m *Movimiento) (float64, error) {
	return c.importeFinalMora(m, false)
}

func (c *Calculadora) importeFinalMora(m *Movimiento, conMora bool) (float64, error) {
	base, cobrado, err := c.montoBaseMora(m, conMora)
	if err != nil || cobrado {
		return base, err
	}
	iva, err := c.importeIvaMora(m, conMora)
	if err != nil {
		return 0, err
	}
	return base + iva, nil
}

// EstaPagada: tiene algo acreditado en importe_pagado.
func EstaPagada(m *Movimiento) bool {
	return m.ImportePagado.Valid && m.ImportePagado.Float64 > 0
}

// DiasParaVencer hasta el primer vencimiento (negativo si ya venció); ok es
// false si no tiene vencimiento cargado.
func (c *Calculadora) DiasParaVencer(m *Movimiento) (dias int, ok bool) {
	if !m.Fecha1erVencimiento.Valid {
		return 0, false
	}
	v := m.Fecha1erVencimiento.Time
	venc := time.Date(v.Year(), v.Month(), v.Day(), 0, 0, 0, 0, bd.Zona)
	return int(math.Round(venc.Sub(c.inicioDelDia()).Hours() / 24)), true
}

func (c *Calculadora) EstaVencida(m *Movimiento) bool {
	if EstaPagada(m) {
		return false
	}
	dias, ok := c.DiasParaVencer(m)
	return ok && dias < 0
}

// EstadoMovimiento: pagada, vencida, proxima (vence en <= 30 días) o pendiente.
func (c *Calculadora) EstadoMovimiento(m *Movimiento) string {
	if EstaPagada(m) {
		return "pagada"
	}
	dias, ok := c.DiasParaVencer(m)
	switch {
	case !ok:
		return "pendiente"
	case dias < 0:
		return "vencida"
	case dias <= 30:
		return "proxima"
	}
	return "pendiente"
}

// EstadoInmueble: al_dia sin deuda, vencida si algún pendiente venció,
// proxima en otro caso.
func (c *Calculadora) EstadoInmueble(pendientes []*Movimiento) string {
	if len(pendientes) == 0 {
		return "al_dia"
	}
	for _, m := range pendientes {
		if c.EstaVencida(m) {
			return "vencida"
		}
	}
	return "proxima"
}

// Centavos convierte un importe a centavos enteros como
// intval(str_replace('.', ”, number_format($x, 2, '.', ”))).
func Centavos(x float64) int64 {
	return int64(math.Round(RedondearPHP(x, 2) * 100))
}

// RedondearPHP replica round() de PHP: pre-redondeo a 15 cifras
// significativas y redondeo "half away from zero".
func RedondearPHP(x float64, decimales int) float64 {
	f := math.Pow(10, float64(decimales))
	v, _ := strconv.ParseFloat(strconv.FormatFloat(x*f, 'g', 15, 64), 64)
	return math.Round(v) / f
}
