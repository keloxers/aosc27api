package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"aosc_api/internal/db"
	"aosc_api/internal/facturacion"
	"aosc_api/internal/pdf"
	"aosc_api/internal/wee"
)

// movimientoJSON es MovimientoResource.
func movimientoJSON(calc *facturacion.Calculadora, m *facturacion.Movimiento) (map[string]any, error) {
	final, err := calc.ImporteFinal(m)
	if err != nil {
		return nil, err
	}
	intereses, err := calc.Intereses(m)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":                    m.ID,
		"inmueble_id":           nuloInt(m.InmueblesID),
		"descripcion":           nulo(m.Movimiento),
		"periodo_mes":           nuloInt(m.PeriodoMes),
		"periodo_anio":          nuloInt(m.PeriodoAnio),
		"consumo":               nuloInt(m.Consumo),
		"fecha_1er_vencimiento": nuloFecha(m.Fecha1erVencimiento),
		"fecha_2do_vencimiento": nuloFecha(m.Fecha2doVencimiento),
		"importe_apagar":        m.ImporteApagar.Float64,
		"importe_final":         final,
		"intereses":             intereses,
		"estado":                calc.EstadoMovimiento(m),
	}, nil
}

func (s *Servidor) facturasIndex(w http.ResponseWriter, r *http.Request) {
	inm, ok := s.inmuebleDeRuta(w, r)
	if !ok || !s.autorizarInmueble(w, r, inm) {
		return
	}
	ctx := r.Context()
	pendientes, err := facturacion.Pendientes(ctx, s.db, inm.ID)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	calc := facturacion.NuevaCalculadora(ctx, s.db)
	data := []map[string]any{}
	for _, m := range pendientes {
		j, err := movimientoJSON(calc, m)
		if err != nil {
			errorInterno(w, r, err)
			return
		}
		data = append(data, j)
	}
	responderJSON(w, http.StatusOK, map[string]any{"data": data})
}

// facturaDeRuta resuelve {inmueble} y {movimiento}, autoriza y verifica que
// la factura sea de ese inmueble.
func (s *Servidor) facturaDeRuta(w http.ResponseWriter, r *http.Request) (*facturacion.Movimiento, bool) {
	inm, ok := s.inmuebleDeRuta(w, r)
	if !ok {
		return nil, false
	}
	id, ok := idDeRuta(r, "movimiento")
	var m *facturacion.Movimiento
	if ok {
		var err error
		if m, err = facturacion.BuscarMovimiento(r.Context(), s.db, id); err != nil {
			errorInterno(w, r, err)
			return nil, false
		}
	}
	if m == nil {
		mensaje(w, http.StatusNotFound, fmt.Sprintf("No query results for model [App\\Models\\Movimiento] %s", r.PathValue("movimiento")))
		return nil, false
	}
	if !s.autorizarInmueble(w, r, inm) {
		return nil, false
	}
	if !m.InmueblesID.Valid || m.InmueblesID.Int64 != inm.ID {
		mensaje(w, http.StatusNotFound, "No encontrado.")
		return nil, false
	}
	return m, true
}

// facturasShow es FacturaDetalleResource: el desglose del modal "Conceptos
// Facturados" del portal.
func (s *Servidor) facturasShow(w http.ResponseWriter, r *http.Request) {
	m, ok := s.facturaDeRuta(w, r)
	if !ok {
		return
	}
	calc := facturacion.NuevaCalculadora(r.Context(), s.db)

	importeConsumo := m.ImporteConsumo
	if importeConsumo <= 0 {
		importeConsumo = m.ImporteApagar.Float64 - m.CargoFijo - m.ImporteCloaca - m.ImporteIva.Float64
	}

	iva, err := calc.ImporteIva(m)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	intereses, err := calc.Intereses(m)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	descuento, _, err := calc.Descuentos(m)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	lista, err := calc.Conceptos(m)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	conceptos := []map[string]any{}
	for _, c := range lista {
		conceptos = append(conceptos, map[string]any{"nombre": c.Nombre, "importe": c.Importe})
	}
	final, err := calc.ImporteFinal(m)
	if err != nil {
		errorInterno(w, r, err)
		return
	}

	responderJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"id":                    m.ID,
		"descripcion":           nulo(m.Movimiento),
		"periodo_mes":           nuloInt(m.PeriodoMes),
		"periodo_anio":          nuloInt(m.PeriodoAnio),
		"fecha_1er_vencimiento": nuloFecha(m.Fecha1erVencimiento),
		"fecha_2do_vencimiento": nuloFecha(m.Fecha2doVencimiento),
		"estado":                calc.EstadoMovimiento(m),
		"cargo_fijo":            m.CargoFijo,
		"importe_consumo":       importeConsumo,
		"importe_cloaca":        m.ImporteCloaca,
		"importe_iva":           iva,
		"intereses":             intereses,
		"descuento":             descuento,
		"conceptos":             conceptos,
		"importe_apagar":        m.ImporteApagar.Float64,
		"importe_final":         final,
	}})
}

func (s *Servidor) facturasPdf(w http.ResponseWriter, r *http.Request) {
	m, ok := s.facturaDeRuta(w, r)
	if !ok {
		return
	}
	contenido, err := s.pdf.Generar(r.Context(), m)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+pdf.NombreArchivo(m)+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(contenido)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contenido)
}

// consumos: últimas 12 mediciones, de la más antigua a la más reciente.
func (s *Servidor) consumos(w http.ResponseWriter, r *http.Request) {
	inm, ok := s.inmuebleDeRuta(w, r)
	if !ok || !s.autorizarInmueble(w, r, inm) {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT periodo_mes, periodo_anio, consumo FROM medicions
		WHERE inmuebles_id = ? ORDER BY id DESC LIMIT 12`, inm.ID)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	defer rows.Close()

	lista := []map[string]any{}
	for rows.Next() {
		var mes, anio, consumo sql.NullInt64
		if err := rows.Scan(&mes, &anio, &consumo); err != nil {
			errorInterno(w, r, err)
			return
		}
		lista = append(lista, map[string]any{
			"periodo_mes":  nuloInt(mes),
			"periodo_anio": nuloInt(anio),
			"consumo":      float64(consumo.Int64),
		})
	}
	if err := rows.Err(); err != nil {
		errorInterno(w, r, err)
		return
	}
	for i, j := 0, len(lista)-1; i < j; i, j = i+1, j-1 {
		lista[i], lista[j] = lista[j], lista[i]
	}
	responderJSON(w, http.StatusOK, lista)
}

// pagos: últimos 20 pagos acreditados del inmueble, más recientes primero.
func (s *Servidor) pagos(w http.ResponseWriter, r *http.Request) {
	inm, ok := s.inmuebleDeRuta(w, r)
	if !ok || !s.autorizarInmueble(w, r, inm) {
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT crd.id, crd.movimientos_id, m.periodo_mes, m.periodo_anio, crd.importe_pagado, cr.fecha
		FROM cajasrecibosdetalles crd
		LEFT JOIN movimientos m ON m.id = crd.movimientos_id
		LEFT JOIN cajasrecibos cr ON cr.id = crd.cajasrecibos_id
		WHERE crd.movimientos_id IN (SELECT id FROM movimientos WHERE inmuebles_id = ?)
		  AND crd.importe_pagado > 0
		ORDER BY crd.id DESC LIMIT 20`, inm.ID)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	defer rows.Close()

	lista := []map[string]any{}
	for rows.Next() {
		var id, movimientoID int64
		var mes, anio sql.NullInt64
		var importe sql.NullFloat64
		var fecha sql.NullTime
		if err := rows.Scan(&id, &movimientoID, &mes, &anio, &importe, &fecha); err != nil {
			errorInterno(w, r, err)
			return
		}
		lista = append(lista, map[string]any{
			"id":             id,
			"movimiento_id":  movimientoID,
			"periodo_mes":    nuloInt(mes),
			"periodo_anio":   nuloInt(anio),
			"importe_pagado": importe.Float64,
			"fecha":          nuloFecha(fecha),
		})
	}
	if err := rows.Err(); err != nil {
		errorInterno(w, r, err)
		return
	}
	responderJSON(w, http.StatusOK, lista)
}

func (s *Servidor) checkout(w http.ResponseWriter, r *http.Request) {
	in, err := leerEntrada(r)
	if err != nil {
		mensaje(w, http.StatusBadRequest, err.Error())
		return
	}
	errs := errores{}
	var ids []int64
	lista, esLista := in["movimiento_ids"].([]any)
	switch {
	case in.vacio("movimiento_ids"):
		errs.agregar("movimiento_ids", "El campo movimiento ids es obligatorio.")
	case !esLista:
		errs.agregar("movimiento_ids", "El campo movimiento ids debe ser un conjunto.")
	default:
		for i, v := range lista {
			id, ok := entero(v)
			if !ok {
				campo := fmt.Sprintf("movimiento_ids.%d", i)
				errs.agregar(campo, fmt.Sprintf("El campo %s debe ser un número entero.", campo))
				continue
			}
			ids = append(ids, id)
		}
	}
	if errs.responder(w) {
		return
	}

	ctx := r.Context()
	u := sesionDe(r).Usuario
	admin, err := s.auth.TienePermiso(ctx, u.ID, "autogestion.admin")
	if err != nil {
		errorInterno(w, r, err)
		return
	}

	res, err := s.cobros.CrearCheckout(ctx, wee.Pagador{ID: u.ID, Nombre: u.Name, Email: u.Email}, ids, admin)
	var errCheckout *wee.ErrCheckout
	if errors.As(err, &errCheckout) {
		mensaje(w, http.StatusUnprocessableEntity, errCheckout.Mensaje)
		return
	}
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	responderJSON(w, http.StatusOK, res)
}

// entero replica la regla `integer` de Laravel (acepta "12" y 12).
func entero(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		n, err := strconv.ParseInt(x.String(), 10, 64)
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func (s *Servidor) checkoutStatus(w http.ResponseWriter, r *http.Request) {
	estado, transaccion, existe, propio, err := s.cobros.EstadoPago(r.Context(), sesionDe(r).Usuario.ID, r.PathValue("ref"))
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	if !existe {
		mensaje(w, http.StatusNotFound, "No encontrado.")
		return
	}
	if !propio {
		mensaje(w, http.StatusForbidden, "Forbidden")
		return
	}
	responderJSON(w, http.StatusOK, map[string]any{
		"estado":      nulo(estado),
		"transaccion": nulo(transaccion),
	})
}

func (s *Servidor) dispositivosStore(w http.ResponseWriter, r *http.Request) {
	in, err := leerEntrada(r)
	if err != nil {
		mensaje(w, http.StatusBadRequest, err.Error())
		return
	}
	errs := errores{}
	token, esTexto := in.cadena("token")
	switch {
	case in.vacio("token"):
		errs.agregar("token", "El campo token es obligatorio.")
	case !esTexto:
		errs.agregar("token", "El campo token debe ser una cadena de caracteres.")
	case len([]rune(token)) > 255:
		errs.agregar("token", "El token no debe ser mayor que 255 caracteres.")
	}
	var plataforma any
	if !in.vacio("plataforma") {
		p, _ := in.texto("plataforma")
		if p != "android" && p != "ios" {
			errs.agregar("plataforma", "La plataforma es inválida.")
		}
		plataforma = p
	}
	if errs.responder(w) {
		return
	}

	// Instancia sin push: se acepta sin guardar para que la app no falle.
	if !s.perfil.Push {
		mensaje(w, http.StatusOK, "Dispositivo registrado.")
		return
	}

	// Un token pertenece a un solo teléfono: si otro usuario inicia sesión
	// en ese teléfono, el token pasa a él.
	ahora := db.Ahora()
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO dispositivos (users_id, token, plataforma, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE users_id = VALUES(users_id), plataforma = VALUES(plataforma), updated_at = VALUES(updated_at)`,
		sesionDe(r).Usuario.ID, token, plataforma, ahora, ahora); err != nil {
		errorInterno(w, r, err)
		return
	}
	mensaje(w, http.StatusOK, "Dispositivo registrado.")
}

func (s *Servidor) dispositivosDestroy(w http.ResponseWriter, r *http.Request) {
	in, err := leerEntrada(r)
	if err != nil {
		mensaje(w, http.StatusBadRequest, err.Error())
		return
	}
	token, esTexto := in.cadena("token")
	errs := errores{}
	if in.vacio("token") {
		errs.agregar("token", "El campo token es obligatorio.")
	} else if !esTexto {
		errs.agregar("token", "El campo token debe ser una cadena de caracteres.")
	}
	if errs.responder(w) {
		return
	}
	if !s.perfil.Push {
		mensaje(w, http.StatusOK, "Dispositivo eliminado.")
		return
	}
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM dispositivos WHERE token = ? AND users_id = ?",
		token, sesionDe(r).Usuario.ID); err != nil {
		errorInterno(w, r, err)
		return
	}
	mensaje(w, http.StatusOK, "Dispositivo eliminado.")
}
