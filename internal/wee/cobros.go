package wee

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

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

func (c *Cobros) externalReferenceUnico(ctx context.Context) (string, error) {
	for {
		code := CodigoAleatorio(10)
		var n int
		if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM payment_intent WHERE external_id = ?", code).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return code, nil
		}
	}
}

func (c *Cobros) log(ctx context.Context, userID int64, texto string) {
	ahora := time.Now().UTC().Format("2006-01-02 15:04:05")
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

// CrearCheckout arma el recibo y sus detalles para las facturas elegidas y
// crea el checkout en Wee (WeeCheckoutService::crear).
func (c *Cobros) CrearCheckout(ctx context.Context, pagador Pagador, movimientoIDs []int64, esAdmin bool) (*ResultadoCheckout, error) {
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
	if endpoint == nil || userWee == nil {
		return nil, fmt.Errorf("faltan las configuraciones wee_endpoint / wee_user_id")
	}

	calc := facturacion.NuevaCalculadora(ctx, c.db)
	hoy := calc.Hoy()
	ahora := calc.Ahora.Format("2006-01-02 15:04:05")

	reciboID, err := c.crearCajasrecibo(ctx, pagador.ID, userWee.Numero.Int64, hoy, ahora)
	if err != nil {
		return nil, err
	}

	var total int64
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
		total += centavos
		items = append(items, Item{
			Quantity:    1,
			Description: m.Movimiento.String,
			Identifier:  fmt.Sprintf("%05d", m.ID),
			Amount:      centavos,
		})

		// Vencido el primero, se cobra con el segundo vencimiento
		// (strtotime(fecha) < time() en PHP, por eso el mismo día ya cuenta).
		vencimiento := m.Fecha1erVencimiento
		if !m.Fecha1erVencimiento.Valid || m.Fecha1erVencimiento.Time.Before(calc.Ahora) {
			vencimiento = m.Fecha2doVencimiento
		}

		if _, err := c.db.ExecContext(ctx, `
			INSERT INTO cajasrecibosdetalles (cajasrecibos_id, movimientos_id, fecha_vencimiento, periodo_mes,
				periodo_anio, cajasrecibosdetalle, consumo, importe_apagar, importe_interes, importe_descuento,
				importe_pagado, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			reciboID, m.ID, fechaONull(vencimiento), m.PeriodoMes, m.PeriodoAnio, m.Movimiento, m.Consumo,
			final, intereses, descuentos, ahora, ahora); err != nil {
			return nil, err
		}
	}

	if _, err := c.db.ExecContext(ctx, "UPDATE cajasrecibos SET importe = ?, updated_at = ? WHERE id = ?",
		float64(total)/100, ahora, reciboID); err != nil {
		return nil, err
	}

	ref, err := c.externalReferenceUnico(ctx)
	if err != nil {
		return nil, err
	}

	anular := func(causa error) error {
		if _, err := c.db.ExecContext(ctx, "UPDATE cajasrecibos SET estado = 'anulada', updated_at = ? WHERE id = ?",
			time.Now().UTC().Format("2006-01-02 15:04:05"), reciboID); err != nil {
			slog.Error("no se pudo anular el recibo", "cajasrecibos_id", reciboID, "error", err)
		}
		c.log(ctx, 1, "Checkout error: "+causa.Error())
		return &ErrCheckout{"No se pudo iniciar el pago, intenta de nuevo más tarde."}
	}

	if !c.habilitado {
		return nil, anular(fmt.Errorf("Wee deshabilitado (WEE_HABILITADO=false)"))
	}

	uuid, checkoutURL, err := c.cliente.Checkout(ctx, endpoint.Texto.String, apikey, Pedido{
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
		return nil, anular(err)
	}

	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO payment_intent (payment_id, external_id, cajasrecibos_id, estado, transaccion,
			importe_pagado, checkout_url, created_at, updated_at)
		VALUES (?, ?, ?, 'pagando', 'pendiente', ?, ?, ?, ?)`,
		uuid, ref, reciboID, fmt.Sprint(total), checkoutURL, ahora, ahora); err != nil {
		return nil, anular(err)
	}

	return &ResultadoCheckout{CheckoutURL: checkoutURL, ExternalReference: ref, CajasreciboID: reciboID}, nil
}

// resolverMovimientos devuelve las facturas impagas elegidas (de las cuentas
// del usuario, salvo admin) y el inmueble de la primera. A diferencia de
// Laravel, los ids inválidos se ignoran en lugar de cortar con error 500.
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
	for _, id := range ids {
		if vistos[id] {
			continue
		}
		vistos[id] = true

		lista, err := facturacion.ListarMovimientos(ctx, c.db,
			"id = ? AND (importe_pagado < importe_apagar OR importe_pagado IS NULL)", id)
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
		movimientos = append(movimientos, m)

		if inm == nil && m.InmueblesID.Valid {
			if inm, err = facturacion.BuscarInmueble(ctx, c.db, m.InmueblesID.Int64); err != nil {
				return nil, nil, err
			}
		}
	}
	return movimientos, inm, nil
}

// crearCajasrecibo usa la planilla abierta de hoy del usuario de caja de Wee
// (cerrando las de días anteriores) y le agrega un recibo.
func (c *Cobros) crearCajasrecibo(ctx context.Context, userID, userCaja int64, hoy, ahora string) (int64, error) {
	var planillaID int64
	err := c.db.QueryRowContext(ctx, `
		SELECT id FROM cajasplanillas WHERE users_id = ? AND fecha = ? AND estado = 'abierta' ORDER BY id LIMIT 1`,
		userCaja, hoy).Scan(&planillaID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := c.db.ExecContext(ctx, "UPDATE cajasplanillas SET estado = 'cerrada', updated_at = ? WHERE users_id = ? AND estado = 'abierta'",
			ahora, userCaja); err != nil {
			return 0, err
		}
		res, err := c.db.ExecContext(ctx, `
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

	res, err := c.db.ExecContext(ctx, `
		INSERT INTO cajasrecibos (cajasplanillas_id, users_id, fecha, estado, created_at, updated_at)
		VALUES (?, ?, ?, 'cerrada', ?, ?)`, planillaID, userID, hoy, ahora, ahora)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// QR de pago de una factura para el PDF (WeeController::WeeQr). Devuelve
// ok=false si el QR está deshabilitado, la factura venció o Wee falló.
func (c *Cobros) QR(ctx context.Context, calc *facturacion.Calculadora, m *facturacion.Movimiento, inm *facturacion.Inmueble, titular *Pagador) (urlQR, checkoutURL string, ok bool) {
	cfg, err := c.configuracion(ctx, "wee_qr")
	if err != nil || cfg == nil || cfg.Estado.Int64 == 0 {
		return "", "", false
	}
	// strtotime(d-m-Y) < time(): el QR deja de emitirse el mismo día del vencimiento.
	if !m.Fecha1erVencimiento.Valid || m.Fecha1erVencimiento.Time.Before(calc.Ahora) {
		return "", "", false
	}
	if titular == nil || inm == nil || !c.habilitado {
		return "", "", false
	}

	falla := func(e error) (string, string, bool) {
		c.log(ctx, titular.ID, "Qr error: "+e.Error())
		return "", "", false
	}

	final, err := calc.ImporteFinal(m)
	if err != nil {
		return falla(err)
	}
	total := facturacion.Centavos(final)

	paymentType, apikey, err := c.datosWeeDistrito(ctx, inm)
	if err != nil {
		return falla(err)
	}
	endpoint, err := c.configuracion(ctx, "wee_endpoint_qr")
	if err != nil || endpoint == nil {
		return falla(fmt.Errorf("falta la configuración wee_endpoint_qr"))
	}
	ref, err := c.externalReferenceUnico(ctx)
	if err != nil {
		return falla(err)
	}

	uuid, urlQR, checkoutURL, err := c.cliente.QRFactura(ctx, endpoint.Texto.String, apikey, Pedido{
		PayerName:         titular.Nombre,
		Description:       c.descripcion,
		FirstTotal:        total,
		FirstDueDate:      m.Fecha1erVencimiento.Time.Format("02-01-2006"),
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

	ahora := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO payment_intent (payment_id, external_id, cajasrecibos_id, movimientos_id, estado, transaccion,
			importe_pagado, checkout_url, created_at, updated_at)
		VALUES (?, ?, NULL, ?, 'qr', 'pendiente', ?, ?, ?, ?)`,
		uuid, ref, m.ID, fmt.Sprint(total), urlQR, ahora, ahora); err != nil {
		return falla(err)
	}
	if _, err := c.db.ExecContext(ctx, "UPDATE movimientos SET qr_pago = ?, qr_checkout = ?, updated_at = ? WHERE id = ?",
		urlQR, checkoutURL, ahora, m.ID); err != nil {
		return falla(err)
	}
	return urlQR, checkoutURL, true
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
