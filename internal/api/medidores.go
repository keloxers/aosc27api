package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"aosc_api/internal/db"
	"aosc_api/internal/facturacion"
)

// inmuebleJSON es InmuebleResource.
func (s *Servidor) inmuebleJSON(r *http.Request, calc *facturacion.Calculadora, inm *facturacion.Inmueble) (map[string]any, error) {
	ctx := r.Context()
	var barrio, calle, distrito sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT b.barrio, c.calle, d.distrito
		FROM inmuebles i
		LEFT JOIN barrios b ON b.id = i.barrios_id
		LEFT JOIN calles c ON c.id = i.calles_id
		LEFT JOIN distritos d ON d.id = i.distritos_id
		WHERE i.id = ?`, inm.ID).Scan(&barrio, &calle, &distrito); err != nil {
		return nil, err
	}

	// direccionCorta(): barrio + calle + "n° altura", omitiendo vacíos.
	var partes []string
	for _, p := range []string{barrio.String, calle.String} {
		if p != "" && p != "0" {
			partes = append(partes, p)
		}
	}
	if inm.Altura.String != "" && inm.Altura.String != "0" {
		partes = append(partes, "n° "+inm.Altura.String)
	}

	pendientes, err := facturacion.Pendientes(ctx, s.db, inm.ID)
	if err != nil {
		return nil, err
	}
	total := 0.0
	for _, m := range pendientes {
		f, err := calc.ImporteFinal(m)
		if err != nil {
			return nil, err
		}
		total += f
	}

	return map[string]any{
		"id":                  inm.ID,
		"cuenta":              nulo(inm.Cuenta),
		"codigo_codex":        nulo(inm.CodigoCodex),
		"direccion":           strings.TrimSpace(strings.Join(partes, " ")),
		"distrito":            nulo(distrito),
		"total_pendiente":     total,
		"cantidad_pendientes": len(pendientes),
		"estado":              calc.EstadoInmueble(pendientes),
	}, nil
}

func (s *Servidor) medidoresIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ids, err := facturacion.InmueblesDelUsuario(ctx, s.db, sesionDe(r).Usuario.ID)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	inmuebles, err := facturacion.ListarInmuebles(ctx, s.db, ids)
	if err != nil {
		errorInterno(w, r, err)
		return
	}

	calc := facturacion.NuevaCalculadora(ctx, s.db)
	data := []map[string]any{}
	for _, inm := range inmuebles {
		j, err := s.inmuebleJSON(r, calc, inm)
		if err != nil {
			errorInterno(w, r, err)
			return
		}
		data = append(data, j)
	}
	responderJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Servidor) medidoresStore(w http.ResponseWriter, r *http.Request) {
	in, err := leerEntrada(r)
	if err != nil {
		mensaje(w, http.StatusBadRequest, err.Error())
		return
	}
	errs := errores{}
	cuenta, _ := in.texto("numero_cuenta")
	if in.vacio("numero_cuenta") {
		errs.agregar("numero_cuenta", "El campo número de cuenta es obligatorio.")
	}
	codigo, _ := in.texto("codigo")
	if in.vacio("codigo") {
		errs.agregar("codigo", "El campo código es obligatorio.")
	}
	if errs.responder(w) {
		return
	}

	ctx := r.Context()
	userID := sesionDe(r).Usuario.ID

	inm, err := facturacion.BuscarInmueblePorCuenta(ctx, s.db, cuenta, codigo)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	if inm == nil {
		mensaje(w, http.StatusUnprocessableEntity, "No hemos encontrado ningún medidor que coincida con los datos proporcionados.")
		return
	}

	ids, err := facturacion.InmueblesDelUsuario(ctx, s.db, userID)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	for _, id := range ids {
		if id == inm.ID {
			mensaje(w, http.StatusUnprocessableEntity, "Este medidor ya está asociado a su cuenta.")
			return
		}
	}

	ahora := db.Ahora()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO usersinmuebles (users_id, inmuebles_id, activo, usersid_ultimo, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, ?)`, userID, inm.ID, userID, ahora, ahora); err != nil {
		errorInterno(w, r, err)
		return
	}

	j, err := s.inmuebleJSON(r, facturacion.NuevaCalculadora(ctx, s.db), inm)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	responderJSON(w, http.StatusOK, map[string]any{"data": j})
}

func (s *Servidor) medidoresDestroy(w http.ResponseWriter, r *http.Request) {
	inm, ok := s.inmuebleDeRuta(w, r)
	if !ok {
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		"DELETE FROM usersinmuebles WHERE inmuebles_id = ? AND users_id = ? ORDER BY id LIMIT 1",
		inm.ID, sesionDe(r).Usuario.ID)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		mensaje(w, http.StatusNotFound, "No encontramos el medidor que quieres desvincular.")
		return
	}
	sinContenido(w)
}

// inmuebleDeRuta resuelve {inmueble} como el route model binding (404).
func (s *Servidor) inmuebleDeRuta(w http.ResponseWriter, r *http.Request) (*facturacion.Inmueble, bool) {
	id, ok := idDeRuta(r, "inmueble")
	var inm *facturacion.Inmueble
	var err error
	if ok {
		inm, err = facturacion.BuscarInmueble(r.Context(), s.db, id)
		if err != nil {
			errorInterno(w, r, err)
			return nil, false
		}
	}
	if inm == nil {
		mensaje(w, http.StatusNotFound, fmt.Sprintf("No query results for model [App\\Models\\Inmueble] %s", r.PathValue("inmueble")))
		return nil, false
	}
	return inm, true
}

// autorizarInmueble: admins ven todo; el resto, sus inmuebles propios o
// vinculados (403 si no).
func (s *Servidor) autorizarInmueble(w http.ResponseWriter, r *http.Request, inm *facturacion.Inmueble) bool {
	ctx := r.Context()
	userID := sesionDe(r).Usuario.ID

	admin, err := s.auth.TienePermiso(ctx, userID, "autogestion.admin")
	if err != nil {
		errorInterno(w, r, err)
		return false
	}
	if admin {
		return true
	}

	ids, err := facturacion.InmueblesDelUsuario(ctx, s.db, userID)
	if err != nil {
		errorInterno(w, r, err)
		return false
	}
	for _, id := range ids {
		if id == inm.ID {
			return true
		}
	}
	mensaje(w, http.StatusForbidden, "Este medidor no está asociado a su cuenta.")
	return false
}

func nulo(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func nuloInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nuloFecha(v sql.NullTime) any {
	if !v.Valid {
		return nil
	}
	return v.Time.Format("2006-01-02")
}
