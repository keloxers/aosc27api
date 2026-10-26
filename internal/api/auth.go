package api

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"aosc_api/internal/auth"
	"aosc_api/internal/db"
)

func (s *Servidor) register(w http.ResponseWriter, r *http.Request) {
	in, err := leerEntrada(r)
	if err != nil {
		mensaje(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	errs := errores{}

	unico := func(columna, valor string) (bool, error) {
		var n int
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE "+columna+" = ?", valor).Scan(&n)
		return n == 0, err
	}

	email, _ := in.texto("email")
	if in.vacio("email") {
		errs.agregar("email", "El campo correo electrónico es obligatorio.")
	} else if !reEmail.MatchString(email) {
		errs.agregar("email", "El correo electrónico no es un correo válido.")
	} else if ok, err := unico("email", email); err != nil {
		errorInterno(w, r, err)
		return
	} else if !ok {
		errs.agregar("email", "El correo electrónico ya ha sido registrado.")
	}

	nombre, esTexto := in.cadena("nombre")
	if in.vacio("nombre") {
		errs.agregar("nombre", "El campo nombre es obligatorio.")
	} else if !esTexto {
		errs.agregar("nombre", "El campo nombre debe ser una cadena de caracteres.")
	} else if utf8.RuneCountInString(nombre) > 255 {
		errs.agregar("nombre", "El nombre no debe ser mayor que 255 caracteres.")
	}

	telefono, _ := in.texto("telefono")
	if in.vacio("telefono") {
		errs.agregar("telefono", "El campo teléfono es obligatorio.")
	} else {
		if !esNumerico(telefono) {
			errs.agregar("telefono", "El teléfono debe ser numérico.")
		}
		if !reDigitos.MatchString(telefono) || len(telefono) < 7 || len(telefono) > 10 {
			errs.agregar("telefono", "El teléfono debe tener entre 7 y 10 dígitos.")
		}
		if ok, err := unico("telefono", telefono); err != nil {
			errorInterno(w, r, err)
			return
		} else if !ok {
			errs.agregar("telefono", "El campo teléfono ya ha sido registrado.")
		}
	}

	contrasena, esTexto := in.cadena("contrasena")
	if in.vacio("contrasena") {
		errs.agregar("contrasena", "El campo contraseña es obligatorio.")
	} else if !esTexto {
		errs.agregar("contrasena", "El campo contraseña debe ser una cadena de caracteres.")
	} else if utf8.RuneCountInString(contrasena) < 8 {
		errs.agregar("contrasena", "La contraseña debe contener al menos 8 caracteres.")
	}

	cuit, _ := in.texto("cuit")
	if in.vacio("cuit") {
		errs.agregar("cuit", "El campo CUIT es obligatorio.")
	} else {
		if !esNumerico(cuit) {
			errs.agregar("cuit", "El CUIT debe ser numérico.")
		}
		if !reDigitos.MatchString(cuit) || len(cuit) != 11 {
			errs.agregar("cuit", "El CUIT debe tener 11 dígitos.")
		}
		if ok, err := unico("cuit", cuit); err != nil {
			errorInterno(w, r, err)
			return
		} else if !ok {
			errs.agregar("cuit", "El campo CUIT ya ha sido registrado.")
		}
	}

	if in.vacio("terminos") {
		errs.agregar("terminos", "El campo términos es obligatorio.")
	}

	if errs.responder(w) {
		return
	}

	n, err := rand.Int(rand.Reader, big.NewInt(90000000))
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	codigo := int(n.Int64()) + 10000000

	hash, err := auth.HashPassword(contrasena)
	if err != nil {
		errorInterno(w, r, err)
		return
	}

	ahora := db.Ahora()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO users (user, email, name, telefono, password, cuit, codigo, activo, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		email, email, nombre, telefono, hash, cuit, codigo, ahora, ahora); err != nil {
		errorInterno(w, r, err)
		return
	}

	// El usuario ya quedó creado: si el correo falla se registra y se
	// responde igual, puede pedir el reenvío desde el portal.
	if err := s.mailer.EnviarValidacion(email, nombre, codigo); err != nil {
		slog.Error("no se pudo enviar el correo de validación", "email", email, "error", err)
	}

	mensaje(w, http.StatusCreated, "Te registramos con éxito. Te enviamos un correo para validar tu cuenta antes de poder iniciar sesión.")
}

// cadena devuelve el campo si es un string JSON (regla `string` de Laravel).
func (e entrada) cadena(campo string) (string, bool) {
	v, ok := e[campo].(string)
	return v, ok
}

func (s *Servidor) login(w http.ResponseWriter, r *http.Request) {
	in, err := leerEntrada(r)
	if err != nil {
		mensaje(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	errs := errores{}

	email, emailTexto := in.cadena("email")
	if in.vacio("email") {
		errs.agregar("email", "El campo correo electrónico es obligatorio.")
	} else if !emailTexto || !reEmail.MatchString(email) {
		errs.agregar("email", "El correo electrónico no es un correo válido.")
	}
	password, passTexto := in.cadena("password")
	if in.vacio("password") {
		errs.agregar("password", "El campo contraseña es obligatorio.")
	} else if !passTexto {
		errs.agregar("password", "El campo contraseña debe ser una cadena de caracteres.")
	}
	dispositivo, devTexto := in.cadena("device_name")
	if in.vacio("device_name") {
		errs.agregar("device_name", "El campo device name es obligatorio.")
	} else if !devTexto {
		errs.agregar("device_name", "El campo device name debe ser una cadena de caracteres.")
	}
	if errs.responder(w) {
		return
	}

	clave := strings.ToLower(email) + "|" + ipCliente(r)
	if seg, bloqueado := s.limitador.Bloqueado(clave); bloqueado {
		errs.agregar("email", fmt.Sprintf("Demasiados intentos de acceso. Por favor intente nuevamente en %d segundos.", seg))
		errs.responder(w)
		return
	}

	u, err := s.auth.BuscarUsuario(ctx, "email", email)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	if u == nil || !u.Activo {
		s.limitador.Fallo(clave)
		errs.agregar("email", "Tu cuenta se encuentra desactivada.")
		errs.responder(w)
		return
	}

	ok, err := s.auth.VerificarPassword(ctx, u.ID, password)
	if err != nil {
		errorInterno(w, r, err)
		return
	}
	if !ok {
		s.limitador.Fallo(clave)
		errs.agregar("email", "Estas credenciales no coinciden con nuestros registros.")
		errs.responder(w)
		return
	}
	s.limitador.Limpiar(clave)

	token, err := s.auth.Emitir(u.ID, dispositivo)
	if err != nil {
		errorInterno(w, r, err)
		return
	}

	responderJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user":  usuarioJSON(u),
	})
}

func usuarioJSON(u *auth.Usuario) map[string]string {
	return map[string]string{
		"id":    strconv.FormatInt(u.ID, 10),
		"name":  u.Name,
		"email": u.Email,
	}
}

func (s *Servidor) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.Revocar(r.Context(), sesionDe(r)); err != nil {
		errorInterno(w, r, err)
		return
	}
	sinContenido(w)
}

func (s *Servidor) me(w http.ResponseWriter, r *http.Request) {
	u := sesionDe(r).Usuario
	responderJSON(w, http.StatusOK, usuarioJSON(&u))
}
