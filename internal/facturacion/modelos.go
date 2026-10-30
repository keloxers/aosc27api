// Package facturacion porta los cálculos de importes de App\Models\Movimiento
// e Inmueble de aosc_front (importeFinal, intereses, descuentos, recargos,
// IVA y estados). Cualquier cambio de fórmula en Laravel tiene que
// replicarse acá: la app y el portal web deben mostrar los mismos importes.
package facturacion

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type Movimiento struct {
	ID                   int64
	UsersID              sql.NullInt64
	Fecha1erVencimiento  sql.NullTime
	Fecha2doVencimiento  sql.NullTime
	Movimiento           sql.NullString
	PeriodoMes           sql.NullInt64
	PeriodoAnio          sql.NullInt64
	Consumo              sql.NullInt64
	ImporteBruto         sql.NullFloat64
	ImporteIva           sql.NullFloat64
	ImporteApagar        sql.NullFloat64
	ImportePagado        sql.NullFloat64
	CargoFijo            float64
	ImporteCloaca        float64
	ImporteConsumo       float64
	MedicionsID          sql.NullInt64
	InmueblesID          sql.NullInt64
	AplicaInteres        sql.NullBool
	AplicaDescuento      bool
	CreatedAt            sql.NullTime
	PlanspagosID         sql.NullInt64
	FechaPago            sql.NullTime
	FechaLecturaAnterior sql.NullTime
	LecturaAnterior      sql.NullInt64
	FechaLecturaActual   sql.NullTime
	LecturaActual        sql.NullInt64
}

const columnasMovimiento = `id, users_id, fecha_1ervencimiento, fecha_2dovencimiento, movimiento,
	periodo_mes, periodo_anio, consumo, importe_bruto, importe_iva, importe_apagar, importe_pagado,
	cargo_fijo, importe_cloaca, importe_consumo, medicions_id, inmuebles_id, aplica_interes,
	aplica_descuento, created_at, planspagos_id, fecha_pago, fecha_lectura_anterior,
	lectura_anterior, fecha_lectura_actual, lectura_actual`

// CondicionPendiente es el criterio de factura impaga usado en toda la app
// (scopePendientes): importe_pagado <= 0 o null, e importe_apagar > 0.
const CondicionPendiente = `(importe_pagado <= 0 OR importe_pagado IS NULL) AND importe_apagar > 0 AND anulado_at IS NULL`

type scanner interface {
	Scan(dest ...any) error
}

func scanMovimiento(s scanner) (*Movimiento, error) {
	m := &Movimiento{}
	err := s.Scan(&m.ID, &m.UsersID, &m.Fecha1erVencimiento, &m.Fecha2doVencimiento, &m.Movimiento,
		&m.PeriodoMes, &m.PeriodoAnio, &m.Consumo, &m.ImporteBruto, &m.ImporteIva, &m.ImporteApagar,
		&m.ImportePagado, &m.CargoFijo, &m.ImporteCloaca, &m.ImporteConsumo, &m.MedicionsID,
		&m.InmueblesID, &m.AplicaInteres, &m.AplicaDescuento, &m.CreatedAt, &m.PlanspagosID,
		&m.FechaPago, &m.FechaLecturaAnterior, &m.LecturaAnterior, &m.FechaLecturaActual,
		&m.LecturaActual)
	return m, err
}

// BuscarMovimiento devuelve nil si no existe o está anulado.
func BuscarMovimiento(ctx context.Context, db *sql.DB, id int64) (*Movimiento, error) {
	m, err := scanMovimiento(db.QueryRowContext(ctx, "SELECT "+columnasMovimiento+" FROM movimientos WHERE id = ? AND anulado_at IS NULL", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// ListarMovimientos ejecuta `SELECT <columnas> FROM movimientos WHERE <where>`.
func ListarMovimientos(ctx context.Context, db *sql.DB, where string, args ...any) ([]*Movimiento, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+columnasMovimiento+" FROM movimientos WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lista []*Movimiento
	for rows.Next() {
		m, err := scanMovimiento(rows)
		if err != nil {
			return nil, err
		}
		lista = append(lista, m)
	}
	return lista, rows.Err()
}

// Pendientes de un inmueble, en orden de id (como el whereIn de Eloquent).
func Pendientes(ctx context.Context, db *sql.DB, inmuebleIDs ...int64) ([]*Movimiento, error) {
	if len(inmuebleIDs) == 0 {
		return nil, nil
	}
	where, args := In("inmuebles_id", inmuebleIDs)
	return ListarMovimientos(ctx, db, where+" AND "+CondicionPendiente+" ORDER BY id", args...)
}

type Inmueble struct {
	ID           int64
	Cuenta       sql.NullString
	CodigoCodex  sql.NullString
	TipoServicio string
	ExencionsID  sql.NullInt64
	MedidorsID   sql.NullInt64
	UsersID      sql.NullInt64
	DistritosID  sql.NullInt64
	BarriosID    sql.NullInt64
	CallesID     sql.NullInt64
	PreciariosID sql.NullInt64
	DescuentosID sql.NullInt64
	Altura       sql.NullString
	Mz           sql.NullString
	Piso         sql.NullString
	Departamento sql.NullString
}

const columnasInmueble = `id, cuenta, codigo_codex, tipo_servicio, exencions_id, medidors_id, users_id,
	distritos_id, barrios_id, calles_id, preciarios_id, descuentos_id, altura, mz, piso, departamento`

func scanInmueble(s scanner) (*Inmueble, error) {
	i := &Inmueble{}
	err := s.Scan(&i.ID, &i.Cuenta, &i.CodigoCodex, &i.TipoServicio, &i.ExencionsID, &i.MedidorsID,
		&i.UsersID, &i.DistritosID, &i.BarriosID, &i.CallesID, &i.PreciariosID, &i.DescuentosID,
		&i.Altura, &i.Mz, &i.Piso, &i.Departamento)
	return i, err
}

// BuscarInmueble devuelve nil si no existe.
func BuscarInmueble(ctx context.Context, db *sql.DB, id int64) (*Inmueble, error) {
	i, err := scanInmueble(db.QueryRowContext(ctx, "SELECT "+columnasInmueble+" FROM inmuebles WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return i, err
}

// ListarInmuebles devuelve los inmuebles con esos ids, en orden de id.
func ListarInmuebles(ctx context.Context, db *sql.DB, ids []int64) ([]*Inmueble, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	where, args := In("id", ids)
	rows, err := db.QueryContext(ctx, "SELECT "+columnasInmueble+" FROM inmuebles WHERE "+where+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lista []*Inmueble
	for rows.Next() {
		i, err := scanInmueble(rows)
		if err != nil {
			return nil, err
		}
		lista = append(lista, i)
	}
	return lista, rows.Err()
}

// BuscarInmueblePorCuenta busca por número de cuenta y código codex (alta
// de medidor desde la app). Devuelve nil si no existe.
func BuscarInmueblePorCuenta(ctx context.Context, db *sql.DB, cuenta, codigo string) (*Inmueble, error) {
	i, err := scanInmueble(db.QueryRowContext(ctx,
		"SELECT "+columnasInmueble+" FROM inmuebles WHERE cuenta = ? AND codigo_codex = ? ORDER BY id LIMIT 1",
		cuenta, codigo))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return i, err
}

// InmueblesDelUsuario: propios (inmuebles.users_id) más los vinculados en
// usersinmuebles, como idsDelUsuario() de los controladores de la API.
func InmueblesDelUsuario(ctx context.Context, db *sql.DB, userID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM inmuebles WHERE users_id = ?
		UNION ALL
		SELECT inmuebles_id FROM usersinmuebles WHERE users_id = ?`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	vistos := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !vistos[id] {
			vistos[id] = true
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// In arma "columna IN (?, ?, ...)" con sus argumentos.
func In(columna string, ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return columna + " IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + ")", args
}
