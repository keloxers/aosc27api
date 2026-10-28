// Package notificaciones evalúa las reglas configuradas en aoscback
// (notificaciones_reglas) y manda las push de facturas pendientes y
// vencimientos. Es el port del comando `notificaciones:enviar` de
// aosc_front: corre cada hora, una regla se procesa a partir de su `hora` y
// cada aviso sale una sola vez por día gracias a notificaciones_envios.
package notificaciones

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	bd "aosc_api/internal/db"
	"aosc_api/internal/facturacion"
	"aosc_api/internal/fcm"
)

const (
	antesVencimiento = "antes_vencimiento"
	diaVencimiento   = "dia_vencimiento"
	vencida          = "vencida"
	diaMes           = "dia_mes"
)

var zona = bd.Zona

type Enviador struct {
	db  *sql.DB
	fcm *fcm.Cliente
}

// NuevoEnviador: fcm puede ser nil solo para simular.
func NuevoEnviador(db *sql.DB, cliente *fcm.Cliente) *Enviador {
	return &Enviador{db: db, fcm: cliente}
}

type Opciones struct {
	ReglaID int64 // 0 = todas las que correspondan a esta hora
	Simular bool
	Salida  func(string) // para --simular; por defecto slog
}

type regla struct {
	ID      int64
	Nombre  string
	Tipo    string
	Dias    sql.NullInt64
	DiaMes  sql.NullInt64
	Titulo  string
	Mensaje string
}

type aviso struct {
	inmuebleID int64
	movimiento *facturacion.Movimiento
	variables  map[string]string
}

type dispositivo struct {
	id    int64
	token string
}

// Ejecutar hace una corrida y devuelve la cantidad de avisos enviados.
func (e *Enviador) Ejecutar(ctx context.Context, o Opciones) (int, error) {
	salida := o.Salida
	if salida == nil {
		salida = func(s string) { slog.Info(s) }
	}
	if !o.Simular && e.fcm == nil {
		return 0, fmt.Errorf("FCM no configurado")
	}

	// Evita corridas superpuestas aunque haya más de una instancia del servicio.
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var lock sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK('aosc_api_notificaciones', 0)").Scan(&lock); err != nil {
		return 0, err
	}
	if lock.Int64 != 1 {
		salida("Hay otra corrida de notificaciones en curso.")
		return 0, nil
	}
	defer conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK('aosc_api_notificaciones')")

	ahora := time.Now().In(zona)
	hoy := time.Date(ahora.Year(), ahora.Month(), ahora.Day(), 0, 0, 0, 0, zona)

	reglas, err := e.reglas(ctx, o.ReglaID, ahora.Hour())
	if err != nil {
		return 0, err
	}
	if len(reglas) == 0 {
		salida("No hay reglas para procesar en este horario.")
		return 0, nil
	}

	usuarios, err := e.usuariosConDispositivo(ctx)
	if err != nil {
		return 0, err
	}

	calc := facturacion.NuevaCalculadora(ctx, e.db)
	enviados := 0

	for _, u := range usuarios {
		ids, err := facturacion.InmueblesDelUsuario(ctx, e.db, u.id)
		if err != nil {
			return enviados, err
		}
		if len(ids) == 0 {
			continue
		}
		inmuebles, err := facturacion.ListarInmuebles(ctx, e.db, ids)
		if err != nil {
			return enviados, err
		}
		cuentas := map[int64]string{}
		var existentes []int64
		for _, i := range inmuebles {
			cuentas[i.ID] = i.Cuenta.String
			existentes = append(existentes, i.ID)
		}
		if len(existentes) == 0 {
			continue
		}

		pendientes, err := facturacion.Pendientes(ctx, e.db, existentes...)
		if err != nil {
			return enviados, err
		}
		if len(pendientes) == 0 {
			continue
		}

		dispositivos, err := e.dispositivos(ctx, u.id)
		if err != nil {
			return enviados, err
		}

		for _, r := range reglas {
			avisos, err := avisosDe(calc, r, pendientes, hoy)
			if err != nil {
				return enviados, err
			}

			for _, a := range avisos {
				var movimientoID sql.NullInt64
				if a.movimiento != nil {
					movimientoID = sql.NullInt64{Int64: a.movimiento.ID, Valid: true}
				}

				var ya int
				if err := e.db.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM notificaciones_envios
					WHERE notificaciones_reglas_id = ? AND users_id = ? AND inmuebles_id = ?
					  AND movimientos_id <=> ? AND fecha = ?`,
					r.ID, u.id, a.inmuebleID, movimientoID, hoy.Format("2006-01-02")).Scan(&ya); err != nil {
					return enviados, err
				}
				if ya > 0 {
					continue
				}

				dias := ""
				if r.Dias.Valid {
					dias = strconv.FormatInt(r.Dias.Int64, 10)
				}
				pares := []string{"{nombre}", u.nombre, "{cuenta}", cuentas[a.inmuebleID], "{dias}", dias}
				for k, v := range a.variables {
					pares = append(pares, k, v)
				}
				reemplazo := strings.NewReplacer(pares...)
				titulo := reemplazo.Replace(r.Titulo)
				mensaje := reemplazo.Replace(r.Mensaje)

				if o.Simular {
					salida(fmt.Sprintf("[%s] usuario %d · cuenta %s: %s — %s", r.Nombre, u.id, cuentas[a.inmuebleID], titulo, mensaje))
					continue
				}

				data := map[string]string{
					"tipo":          r.Tipo,
					"inmueble_id":   strconv.FormatInt(a.inmuebleID, 10),
					"movimiento_id": "",
				}
				if movimientoID.Valid {
					data["movimiento_id"] = strconv.FormatInt(movimientoID.Int64, 10)
				}

				ok := 0
				vigentes := dispositivos[:0:0]
				for _, d := range dispositivos {
					switch e.fcm.Enviar(ctx, d.token, titulo, mensaje, data) {
					case fcm.OK:
						ok++
						vigentes = append(vigentes, d)
					case fcm.TokenInvalido:
						if _, err := e.db.ExecContext(ctx, "DELETE FROM dispositivos WHERE id = ?", d.id); err != nil {
							return enviados, err
						}
					default:
						vigentes = append(vigentes, d)
					}
				}
				dispositivos = vigentes

				// Si no llegó a ningún teléfono no se registra, así se
				// reintenta en la próxima corrida.
				if ok > 0 {
					ts := bd.Ahora()
					if _, err := e.db.ExecContext(ctx, `
						INSERT INTO notificaciones_envios (notificaciones_reglas_id, users_id, inmuebles_id,
							movimientos_id, fecha, dispositivos, created_at, updated_at)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
						r.ID, u.id, a.inmuebleID, movimientoID, hoy.Format("2006-01-02"), ok, ts, ts); err != nil {
						return enviados, err
					}
					enviados++
				}
			}
		}
	}

	if o.Simular {
		salida("Simulación terminada.")
	} else {
		salida(fmt.Sprintf("Notificaciones enviadas: %d.", enviados))
	}
	return enviados, nil
}

// avisosDe: las reglas de vencimiento generan un aviso por factura; la de
// día del mes, uno por cuenta con deuda.
func avisosDe(calc *facturacion.Calculadora, r regla, pendientes []*facturacion.Movimiento, hoy time.Time) ([]aviso, error) {
	if r.Tipo == diaMes {
		// Un día 31 configurado cae el último día en los meses más cortos.
		ultimoDia := time.Date(hoy.Year(), hoy.Month()+1, 0, 0, 0, 0, 0, zona).Day()
		if !r.DiaMes.Valid || r.DiaMes.Int64 == 0 || int64(hoy.Day()) != min(r.DiaMes.Int64, int64(ultimoDia)) {
			return nil, nil
		}

		var orden []int64
		grupos := map[int64][]*facturacion.Movimiento{}
		for _, m := range pendientes {
			id := m.InmueblesID.Int64
			if _, ok := grupos[id]; !ok {
				orden = append(orden, id)
			}
			grupos[id] = append(grupos[id], m)
		}

		var avisos []aviso
		for _, id := range orden {
			facturas := grupos[id]
			total := 0.0
			var fechas []string
			for _, m := range facturas {
				f, err := calc.ImporteFinal(m)
				if err != nil {
					return nil, err
				}
				total += f
				if m.Fecha1erVencimiento.Valid {
					fechas = append(fechas, m.Fecha1erVencimiento.Time.Format("2006-01-02"))
				}
			}
			vencimiento := ""
			if len(fechas) > 0 {
				sort.Strings(fechas)
				t, _ := time.ParseInLocation("2006-01-02", fechas[0], zona)
				vencimiento = t.Format("02/01/2006")
			}
			avisos = append(avisos, aviso{
				inmuebleID: id,
				variables: map[string]string{
					"{cantidad}":    strconv.Itoa(len(facturas)),
					"{importe}":     facturacion.Moneda(total),
					"{vencimiento}": vencimiento,
					"{periodo}":     "",
				},
			})
		}
		return avisos, nil
	}

	var objetivo time.Time
	switch r.Tipo {
	case antesVencimiento:
		objetivo = hoy.AddDate(0, 0, int(r.Dias.Int64))
	case diaVencimiento:
		objetivo = hoy
	case vencida:
		dias := int64(1)
		if r.Dias.Valid {
			dias = max(r.Dias.Int64, 1)
		}
		objetivo = hoy.AddDate(0, 0, -int(dias))
	default:
		return nil, nil
	}
	clave := objetivo.Format("2006-01-02")

	var avisos []aviso
	for _, m := range pendientes {
		if !m.Fecha1erVencimiento.Valid || m.Fecha1erVencimiento.Time.Format("2006-01-02") != clave {
			continue
		}
		f, err := calc.ImporteFinal(m)
		if err != nil {
			return nil, err
		}
		// Las cuotas de plan de pago no tienen período: se usa su descripción.
		periodo := m.Movimiento.String
		if m.PeriodoMes.Int64 != 0 && m.PeriodoAnio.Int64 != 0 {
			periodo = fmt.Sprintf("%d/%d", m.PeriodoMes.Int64, m.PeriodoAnio.Int64)
		}
		avisos = append(avisos, aviso{
			inmuebleID: m.InmueblesID.Int64,
			movimiento: m,
			variables: map[string]string{
				"{cantidad}":    "1",
				"{importe}":     facturacion.Moneda(f),
				"{vencimiento}": m.Fecha1erVencimiento.Time.Format("02/01/2006"),
				"{periodo}":     periodo,
			},
		})
	}
	return avisos, nil
}

func (e *Enviador) reglas(ctx context.Context, reglaID int64, hora int) ([]regla, error) {
	q := "SELECT id, nombre, tipo, dias, dia_mes, titulo, mensaje FROM notificaciones_reglas WHERE estado = 1"
	var args []any
	if reglaID > 0 {
		q += " AND id = ?"
		args = append(args, reglaID)
	} else {
		q += " AND hora <= ?"
		args = append(args, hora)
	}
	rows, err := e.db.QueryContext(ctx, q+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lista []regla
	for rows.Next() {
		var r regla
		if err := rows.Scan(&r.ID, &r.Nombre, &r.Tipo, &r.Dias, &r.DiaMes, &r.Titulo, &r.Mensaje); err != nil {
			return nil, err
		}
		lista = append(lista, r)
	}
	return lista, rows.Err()
}

type usuario struct {
	id     int64
	nombre string
}

func (e *Enviador) usuariosConDispositivo(ctx context.Context) ([]usuario, error) {
	rows, err := e.db.QueryContext(ctx,
		"SELECT id, name FROM users WHERE id IN (SELECT users_id FROM dispositivos) ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lista []usuario
	for rows.Next() {
		var u usuario
		var nombre sql.NullString
		if err := rows.Scan(&u.id, &nombre); err != nil {
			return nil, err
		}
		u.nombre = nombre.String
		lista = append(lista, u)
	}
	return lista, rows.Err()
}

func (e *Enviador) dispositivos(ctx context.Context, userID int64) ([]dispositivo, error) {
	rows, err := e.db.QueryContext(ctx, "SELECT id, token FROM dispositivos WHERE users_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lista []dispositivo
	for rows.Next() {
		var d dispositivo
		if err := rows.Scan(&d.id, &d.token); err != nil {
			return nil, err
		}
		lista = append(lista, d)
	}
	return lista, rows.Err()
}

// Programar corre Ejecutar al comienzo de cada hora hasta que se cancele ctx.
func (e *Enviador) Programar(ctx context.Context) {
	for {
		ahora := time.Now()
		proxima := ahora.Truncate(time.Hour).Add(time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(proxima)):
		}
		n, err := e.Ejecutar(ctx, Opciones{})
		if err != nil {
			slog.Error("falló el envío de notificaciones", "error", err)
			continue
		}
		slog.Info("corrida de notificaciones", "enviados", n)
	}
}
