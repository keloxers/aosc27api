package wee

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"time"

	bd "aosc_api/internal/db"
	"aosc_api/internal/facturacion"
)

// ErrCheckout lleva un mensaje para mostrarle al usuario (422).
type ErrCheckout struct{ Mensaje string }

func (e *ErrCheckout) Error() string { return e.Mensaje }

type Cobros struct {
	db          *sql.DB
	cliente     *Cliente
	habilitado  bool
	descripcion string
}

func NuevoCobros(db *sql.DB, habilitado bool, descripcion string) *Cobros {
	return &Cobros{db: db, cliente: NuevoCliente(), habilitado: habilitado, descripcion: descripcion}
}

type ResultadoCheckout struct {
	CheckoutURL       string `json:"checkout_url"`
	ExternalReference string `json:"external_reference"`
	CajasreciboID     int64  `json:"cajasrecibo_id"`
}

type configuracion struct {
	Numero sql.NullInt64
	Texto  sql.NullString
	Estado sql.NullInt64
}

func (c *Cobros) configuracion(ctx context.Context, clave string) (*configuracion, error) {
	var cfg configuracion
	err := c.db.QueryRowContext(ctx,
		"SELECT numero, texto, estado FROM configuracions WHERE configuracion = ? ORDER BY id LIMIT 1", clave).
		Scan(&cfg.Numero, &cfg.Texto, &cfg.Estado)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &cfg, err
}

func (c *Cobros) datosWeeDistrito(ctx context.Context, inm *facturacion.Inmueble) (paymentType, apikey string, err error) {
	if !inm.DistritosID.Valid {
		return "", "", nil
	}
	var pt, ak sql.NullString
	err = c.db.QueryRowContext(ctx, "SELECT wee_payment_type, wee_apikey FROM distritos WHERE id = ?", inm.DistritosID.Int64).Scan(&pt, &ak)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return pt.String, ak.String, err
}

// ejecutor lo cumplen *sql.DB y *sql.Tx: permite escribir dentro de una transacción.
type ejecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// externalReferenceUnico busca una referencia libre; con cota para no girar
// para siempre si la base responde algo raro.
func (c *Cobros) externalReferenceUnico(ctx context.Context, ex ejecutor) (string, error) {
	for i := 0; i < 20; i++ {
		code := CodigoAleatorio(10)
		var n int
		if err := ex.QueryRowContext(ctx, "SELECT COUNT(*) FROM payment_intent WHERE external_id = ?", code).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return code, nil
		}
	}
	return "", fmt.Errorf("no se pudo generar una referencia externa única")
}

func (c *Cobros) log(ctx context.Context, userID int64, texto string) {
	ahora := bd.Ahora()
	if _, err := c.db.ExecContext(ctx, "INSERT INTO logs (users_id, log, created_at, updated_at) VALUES (?, ?, ?, ?)",
		userID, texto, ahora, ahora); err != nil {
		slog.Error("no se pudo registrar el log", "error", err)
	}
	slog.Warn(texto, "users_id", userID)
}

type Pagador struct {
	ID     int64
	Nombre string
	Email  string
}

// itemCobro es una factura ya calculada, lista para escribir y mandar a Wee.
type itemCobro struct {
	m           *facturacion.Movimiento
	final       float64
	intereses   float64
	descuentos  float64
	centavos    int64
	vencimiento sql.NullTime
}

// maxFacturas acota cuántas facturas se pagan en un solo checkout.
const maxFacturas = 50

// CrearCheckout arma el recibo y sus detalles para las facturas elegidas y
// crea el checkout en Wee (WeeCheckoutService::crear).
func (c *Cobros) CrearCheckout(ctx context.Context, pagador Pagador, movimientoIDs []int64, esAdmin bool) (*ResultadoCheckout, error) {
	// Antes de tocar la base: con el cobro apagado no se deja ni un recibo.
	if !c.habilitado {
		return nil, &ErrCheckout{"El cobro online no está disponible en este momento."}
	}

	movimientos, inm, err := c.resolverMovimientos(ctx, pagador.ID, movimientoIDs, esAdmin)
	if err != nil {
		return nil, err
	}
	if inm == nil {
		return nil, &ErrCheckout{"No se encontraron facturas pendientes válidas para pagar."}
	}

	paymentType, apikey, err := c.datosWeeDistrito(ctx, inm)
	if err != nil {
		return nil, err
	}
	if paymentType == "" || apikey == "" {
		return nil, &ErrCheckout{"El cobro online no está disponible para este distrito."}
	}

	endpoint, err := c.configuracion(ctx, "wee_endpoint")
	if err != nil {
		return nil, err
	}
	userWee, err := c.configuracion(ctx, "wee_user_id")
	if err != nil {
		return nil, err
	}
	if endpoint == nil || userWee == nil || !endpoint.Texto.Valid || endpoint.Texto.String == "" ||
		!userWee.Numero.Valid || userWee.Numero.Int64 <= 0 {
		return nil, fmt.Errorf("faltan las configuraciones wee_endpoint / wee_user_id")
	}

	calc := facturacion.NuevaCalculadora(ctx, c.db)
	hoy := calc.Hoy()
	ahora := calc.Ahora.Format("2006-01-02 15:04:05")

	// Se calcula todo en memoria antes de escribir: si algo falla o no hay
	// importe, no queda ningún recibo a medias.
	var total int64
	calculos := make([]itemCobro, 0, len(movimientos))
	items := []Item{}
	for _, m := range movimientos {
		final, err := calc.ImporteFinal(m)
		if err != nil {
			return nil, err
		}
		intereses, err := calc.Intereses(m)
		if err != nil {
			return nil, err
		}
		descuentos, _, err := calc.Descuentos(m)
		if err != nil {
			return nil, err
		}

		centavos := facturacion.Centavos(final)
		if centavos <= 0 {
			return nil, &ErrCheckout{"Hay facturas sin importe a pagar."}
		}
		total += centavos

		// Vencido el primero, se cobra con el segundo vencimiento
		// (strtotime(fecha) < time() en PHP, por eso el mismo día ya cuenta).
		vencimiento := m.Fecha1erVencimiento
		if !m.Fecha1erVencimiento.Valid || m.Fecha1erVencimiento.Time.Before(calc.Ahora) {
			vencimiento = m.Fecha2doVencimiento
		}

		calculos = append(calculos, itemCobro{m, final, intereses, descuentos, centavos, vencimiento})
		items = append(items, Item{
			Quantity:    1,
			Description: m.Movimiento.String,
			Identifier:  fmt.Sprintf("%05d", m.ID),
			Amount:      centavos,
		})
	}
	if total <= 0 {
		return nil, &ErrCheckout{"Hay facturas sin importe a pagar."}
	}

	reciboID, ref, err := c.registrarCheckout(ctx, pagador.ID, userWee.Numero.Int64, hoy, ahora, total, calculos)
	if err != nil {
		return nil, err
	}

	// Si el cliente móvil corta la conexión, la escritura posterior a Wee no
	// se debe perder: el pago puede haberse creado igual.
	ctxW, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	uuid, checkoutURL, err := c.cliente.Checkout(ctxW, endpoint.Texto.String, apikey, Pedido{
		PayerName:         pagador.Nombre,
		Description:       c.descripcion,
		FirstTotal:        total,
		FirstDueDate:      calc.Ahora.Format("02-01-2006"),
		ExternalReference: ref,
		PayerEmail:        pagador.Email,
		PaymentType:       paymentType,
		Items:             items,
	})
	if err != nil {
		ahoraW := bd.Ahora()
		if _, e := c.db.ExecContext(ctxW, "UPDATE cajasrecibos SET estado = 'anulada', updated_at = ? WHERE id = ?",
			ahoraW, reciboID); e != nil {
			slog.Error("no se pudo anular el recibo", "cajasrecibos_id", reciboID, "error", e)
		}
		if _, e := c.db.ExecContext(ctxW, "UPDATE payment_intent SET transaccion = 'rechazado', estado = 'error', updated_at = ? WHERE external_id = ?",
			ahoraW, ref); e != nil {
			slog.Error("no se pudo marcar el payment_intent como error", "external_id", ref, "error", e)
		}
		c.log(ctxW, 1, "Checkout error: "+err.Error())
		return nil, &ErrCheckout{"No se pudo iniciar el pago, intenta de nuevo más tarde."}
	}

	if _, err := c.db.ExecContext(ctxW, "UPDATE payment_intent SET payment_id = ?, checkout_url = ?, updated_at = ? WHERE external_id = ?",
		uuid, checkoutURL, bd.Ahora(), ref); err != nil {
		// Wee ya creó el checkout: se devuelve igual (el webhook encuentra el
		// pago por external_id). Nunca se loguea la apikey.
		slog.Error("no se pudo guardar el checkout de Wee", "external_id", ref, "uuid", uuid,
			"checkout_url", checkoutURL, "error", err)
	}

	return &ResultadoCheckout{CheckoutURL: checkoutURL, ExternalReference: ref, CajasreciboID: reciboID}, nil
}

// registrarCheckout escribe planilla, recibo, detalles y payment_intent en una
// sola transacción. Toma el lock 'wee-planilla' (compartido con Laravel, que
// también crea planillas del usuario Wee) en una conexión dedicada.
func (c *Cobros) registrarCheckout(ctx context.Context, userID, userCaja int64, hoy, ahora string, total int64, calculos []itemCobro) (reciboID int64, ref string, err error) {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return 0, "", err
	}
	// El lock vive en la conexión: si no se pudo soltar, se descarta la
	// conexión en vez de devolverla al pool con el lock tomado.
	bloqueado := false
	defer func() {
		if bloqueado {
			ctxL, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, e := conn.ExecContext(ctxL, "SELECT RELEASE_LOCK('wee-planilla')"); e != nil {
				slog.Error("no se pudo soltar el lock wee-planilla", "error", e)
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		conn.Close()
	}()

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK('wee-planilla', 10)").Scan(&got); err != nil {
		return 0, "", err
	}
	if !got.Valid || got.Int64 != 1 {
		return 0, "", fmt.Errorf("no se pudo obtener el lock wee-planilla")
	}
	bloqueado = true

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	commiteado := false
	defer func() {
		if !commiteado {
			tx.Rollback()
		}
	}()

	reciboID, err = c.crearCajasrecibo(ctx, tx, userID, userCaja, hoy, ahora)
	if err != nil {
		return 0, "", err
	}

	for _, it := range calculos {
		m := it.m
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO cajasrecibosdetalles (cajasrecibos_id, movimientos_id, fecha_vencimiento, periodo_mes,
				periodo_anio, cajasrecibosdetalle, consumo, importe_apagar, importe_interes, importe_descuento,
				importe_pagado, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			reciboID, m.ID, fechaONull(it.vencimiento), m.PeriodoMes, m.PeriodoAnio, m.Movimiento, m.Consumo,
			it.final, it.intereses, it.descuentos, ahora, ahora); err != nil {
			return 0, "", err
		}
	}

	if _, err := tx.ExecContext(ctx, "UPDATE cajasrecibos SET importe = ?, updated_at = ? WHERE id = ?",
		float64(total)/100, ahora, reciboID); err != nil {
		return 0, "", err
	}

	ref, err = c.externalReferenceUnico(ctx, tx)
	if err != nil {
		return 0, "", err
	}

	// payment_id se completa cuando Wee responde.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO payment_intent (payment_id, external_id, cajasrecibos_id, estado, transaccion,
			importe_pagado, checkout_url, created_at, updated_at)
		VALUES (NULL, ?, ?, 'pagando', 'pendiente', ?, NULL, ?, ?)`,
		ref, reciboID, fmt.Sprint(total), ahora, ahora); err != nil {
		return 0, "", err
	}

	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	commiteado = true
	return reciboID, ref, nil
}

// resolverMovimientos devuelve las facturas impagas elegidas (de las cuentas
// del usuario, salvo admin) y el inmueble de la primera. A diferencia de
// Laravel, los ids inválidos se ignoran en lugar de cortar con error 500.
// Todas deben ser del mismo distrito: se cobra con la apikey de uno solo.
func (c *Cobros) resolverMovimientos(ctx context.Context, userID int64, ids []int64, esAdmin bool) ([]*facturacion.Movimiento, *facturacion.Inmueble, error) {
	var permitidos map[int64]bool
	if !esAdmin {
		propios, err := facturacion.InmueblesDelUsuario(ctx, c.db, userID)
		if err != nil {
			return nil, nil, err
		}
		permitidos = map[int64]bool{}
		for _, id := range propios {
			permitidos[id] = true
		}
	}

	var movimientos []*facturacion.Movimiento
	var inm *facturacion.Inmueble
	vistos := map[int64]bool{}
	inmuebles := map[int64]*facturacion.Inmueble{}
	for _, id := range ids {
		if vistos[id] {
			continue
		}
		vistos[id] = true
		if len(vistos) > maxFacturas {
			return nil, nil, &ErrCheckout{"Podés pagar hasta 50 facturas por vez."}
		}

		lista, err := facturacion.ListarMovimientos(ctx, c.db, "id = ? AND "+facturacion.CondicionPendiente, id)
		if err != nil {
			return nil, nil, err
		}
		if len(lista) == 0 {
			continue
		}
		m := lista[0]
		if !esAdmin && (!m.InmueblesID.Valid || !permitidos[m.InmueblesID.Int64]) {
			continue
		}

		// Sin inmueble no hay distrito (ni apikey) con que cobrarla.
		if !m.InmueblesID.Valid {
			continue
		}
		invM, ok := inmuebles[m.InmueblesID.Int64]
		if !ok {
			if invM, err = facturacion.BuscarInmueble(ctx, c.db, m.InmueblesID.Int64); err != nil {
				return nil, nil, err
			}
			inmuebles[m.InmueblesID.Int64] = invM
		}
		if invM == nil {
			continue
		}
		if inm == nil {
			inm = invM
		}
		if invM.DistritosID != inm.DistritosID {
			return nil, nil, &ErrCheckout{"Las facturas de distintos distritos se pagan por separado."}
		}
		movimientos = append(movimientos, m)
	}
	return movimientos, inm, nil
}

// crearCajasrecibo usa la planilla abierta de hoy del usuario de caja de Wee
// (cerrando las de días anteriores) y le agrega un recibo.
func (c *Cobros) crearCajasrecibo(ctx context.Context, ex ejecutor, userID, userCaja int64, hoy, ahora string) (int64, error) {
	var planillaID int64
	err := ex.QueryRowContext(ctx, `
		SELECT id FROM cajasplanillas WHERE users_id = ? AND fecha = ? AND estado = 'abierta' ORDER BY id LIMIT 1`,
		userCaja, hoy).Scan(&planillaID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := ex.ExecContext(ctx, "UPDATE cajasplanillas SET estado = 'cerrada', updated_at = ? WHERE users_id = ? AND estado = 'abierta'",
			ahora, userCaja); err != nil {
			return 0, err
		}
		res, err := ex.ExecContext(ctx, `
			INSERT INTO cajasplanillas (users_id, fecha, estado, created_at, updated_at) VALUES (?, ?, 'abierta', ?, ?)`,
			userCaja, hoy, ahora, ahora)
		if err != nil {
			return 0, err
		}
		if planillaID, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}

	res, err := ex.ExecContext(ctx, `
		INSERT INTO cajasrecibos (cajasplanillas_id, users_id, fecha, estado, created_at, updated_at)
		VALUES (?, ?, ?, 'cerrada', ?, ?)`, planillaID, userID, hoy, ahora, ahora)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// QR de pago de una factura para el PDF (WeeController::WeeQr). Devuelve
// ok=false si el QR está deshabilitado, la factura ya está pagada o Wee falló.
// El QR no vence: va por el importe sin mora, también pasado el vencimiento.
func (c *Cobros) QR(ctx context.Context, calc *facturacion.Calculadora, m *facturacion.Movimiento, inm *facturacion.Inmueble, titular *Pagador) (urlQR, checkoutURL string, ok bool) {
	cfg, err := c.configuracion(ctx, "wee_qr")
	if err != nil || cfg == nil || cfg.Estado.Int64 == 0 {
		return "", "", false
	}
	if titular == nil || inm == nil || !c.habilitado {
		return "", "", false
	}
	// Sin QR para facturas ya pagadas: se podían volver a pagar desde el PDF.
	if m.ImportePagado.Valid && m.ImportePagado.Float64 > 0 {
		return "", "", false
	}

	falla := func(e error) (string, string, bool) {
		c.log(ctx, titular.ID, "Qr error: "+e.Error())
		return "", "", false
	}

	final, err := calc.ImporteSinMora(m)
	if err != nil {
		return falla(err)
	}
	total := facturacion.Centavos(final)

	paymentType, apikey, err := c.datosWeeDistrito(ctx, inm)
	if err != nil {
		return falla(err)
	}
	// Distrito sin Wee configurado: no hay QR, sin ruido en el log.
	if paymentType == "" || apikey == "" {
		return "", "", false
	}
	endpoint, err := c.configuracion(ctx, "wee_endpoint_qr")
	if err != nil || endpoint == nil || !endpoint.Texto.Valid || endpoint.Texto.String == "" {
		return falla(fmt.Errorf("falta la configuración wee_endpoint_qr"))
	}

	// Cada render del PDF creaba un QR nuevo y pagar dos QR de la misma factura
	// termina en pago duplicado: se reusa el vigente si sigue pendiente y por
	// el mismo importe.
	// El último intent del movimiento: el webhook le cambia `estado` al de Wee, así que
	// solo se reusa si sigue intacto ('qr', pendiente) y es el de qr_pago.
	var estadoIntent, transaccion, importeIntent sql.NullString
	err = c.db.QueryRowContext(ctx,
		"SELECT estado, transaccion, importe_pagado FROM payment_intent WHERE movimientos_id = ? ORDER BY id DESC LIMIT 1", m.ID).
		Scan(&estadoIntent, &transaccion, &importeIntent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return falla(err)
	}
	if err == nil && estadoIntent.String == "qr" && transaccion.String == "pendiente" && importeIntent.String == fmt.Sprint(total) {
		var qrPago, qrCheckout sql.NullString
		if err := c.db.QueryRowContext(ctx, "SELECT qr_pago, qr_checkout FROM movimientos WHERE id = ?", m.ID).
			Scan(&qrPago, &qrCheckout); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return falla(err)
		} else if err == nil && qrPago.String != "" && qrCheckout.String != "" {
			return qrPago.String, qrCheckout.String, true
		}
	}

	ref, err := c.externalReferenceUnico(ctx, c.db)
	if err != nil {
		return falla(err)
	}

	// El PDF espera esta llamada: timeout corto.
	ctxQ, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	uuid, urlQR, checkoutURL, pagoID, err := c.cliente.QRFactura(ctxQ, endpoint.Texto.String, apikey, Pedido{
		PayerName:         titular.Nombre,
		Description:       c.descripcion,
		FirstTotal:        total,
		FirstDueDate:      primerVencimientoQR(m, calc.Ahora),
		ExternalReference: ref,
		PayerEmail:        titular.Email,
		PaymentType:       paymentType,
		Items: []Item{{
			Quantity:    1,
			Description: m.Movimiento.String,
			Identifier:  fmt.Sprintf("%05d", m.ID),
			Amount:      total,
		}},
	})
	if err != nil {
		return falla(err)
	}

	ahora := bd.Ahora()
	var pagoIDSQL any
	if pagoID > 0 {
		pagoIDSQL = pagoID
	}
	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO payment_intent (payment_id, external_id, cajasrecibos_id, movimientos_id, estado, transaccion,
			importe_pagado, checkout_url, wee_pago_id, created_at, updated_at)
		VALUES (?, ?, NULL, ?, 'qr', 'pendiente', ?, ?, ?, ?, ?)`,
		uuid, ref, m.ID, fmt.Sprint(total), urlQR, pagoIDSQL, ahora, ahora); err != nil {
		return falla(err)
	}
	if _, err := c.db.ExecContext(ctx, "UPDATE movimientos SET qr_pago = ?, qr_checkout = ?, updated_at = ? WHERE id = ?",
		urlQR, checkoutURL, ahora, m.ID); err != nil {
		return falla(err)
	}
	return urlQR, checkoutURL, true
}

// primerVencimientoQR: 1er vencimiento (DD-MM-AAAA) si es hoy o futuro, si no hoy.
func primerVencimientoQR(m *facturacion.Movimiento, ahora time.Time) string {
	hoy := ahora.Format("2006-01-02")
	if m.Fecha1erVencimiento.Valid && m.Fecha1erVencimiento.Time.Format("2006-01-02") >= hoy {
		return m.Fecha1erVencimiento.Time.Format("02-01-2006")
	}
	return ahora.Format("02-01-2006")
}

// Resultado resume estado/transaccion de un payment_intent para la app:
// pagado, duplicado, fallido o pendiente.
func Resultado(estado, transaccion sql.NullString) string {
	switch {
	case transaccion.String == "completado":
		return "pagado"
	case estado.String == "DUPLICADO":
		return "duplicado"
	case transaccion.String == "rechazado":
		return "fallido"
	}
	return "pendiente"
}

func fechaONull(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Format("2006-01-02")
}

// EstadoPago (GET /checkout/{ref}/status): nil si no existe; propio es
// false si el recibo no es del usuario.
func (c *Cobros) EstadoPago(ctx context.Context, userID int64, ref string) (estado, transaccion sql.NullString, existe, propio bool, err error) {
	var reciboID sql.NullInt64
	err = c.db.QueryRowContext(ctx,
		"SELECT estado, transaccion, cajasrecibos_id FROM payment_intent WHERE external_id = ? ORDER BY id LIMIT 1", ref).
		Scan(&estado, &transaccion, &reciboID)
	if errors.Is(err, sql.ErrNoRows) {
		return estado, transaccion, false, false, nil
	}
	if err != nil {
		return
	}
	existe = true
	if !reciboID.Valid {
		return
	}
	var dueño int64
	err = c.db.QueryRowContext(ctx, "SELECT users_id FROM cajasrecibos WHERE id = ?", reciboID.Int64).Scan(&dueño)
	if errors.Is(err, sql.ErrNoRows) {
		return estado, transaccion, true, false, nil
	}
	propio = err == nil && dueño == userID
	return
}
