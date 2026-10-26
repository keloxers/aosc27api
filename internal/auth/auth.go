// Package auth emite y valida los JWT de la app y verifica contraseñas
// contra los hashes bcrypt que guarda Laravel en users.password.
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var ErrTokenInvalido = errors.New("token inválido")

type Usuario struct {
	ID     int64
	Name   string
	Email  string
	Activo bool
}

type Servicio struct {
	db     *sql.DB
	secret []byte
	ttl    time.Duration
}

func NuevoServicio(db *sql.DB, secret []byte, ttl time.Duration) *Servicio {
	return &Servicio{db: db, secret: secret, ttl: ttl}
}

// MigracionRevocados crea la tabla de tokens revocados por logout.
const MigracionRevocados = `CREATE TABLE IF NOT EXISTS app_tokens_revocados (
	jti CHAR(32) NOT NULL PRIMARY KEY,
	users_id BIGINT UNSIGNED NOT NULL,
	expira_en DATETIME NOT NULL,
	created_at TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP,
	KEY app_tokens_revocados_expira_en_index (expira_en)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

type claims struct {
	Dispositivo string `json:"dev,omitempty"`
	jwt.RegisteredClaims
}

// Emitir firma un token nuevo para el usuario.
func (s *Servicio) Emitir(userID int64, dispositivo string) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	ahora := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Dispositivo: dispositivo,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(userID, 10),
			ID:        hex.EncodeToString(jti),
			IssuedAt:  jwt.NewNumericDate(ahora),
			ExpiresAt: jwt.NewNumericDate(ahora.Add(s.ttl)),
			Issuer:    "aosc_api",
		},
	})
	return t.SignedString(s.secret)
}

// Sesion es el resultado de validar un token.
type Sesion struct {
	Usuario  Usuario
	JTI      string
	ExpiraEn time.Time
}

// Validar verifica firma, vencimiento y que el token no haya sido revocado,
// y carga el usuario.
func (s *Servicio) Validar(ctx context.Context, token string) (*Sesion, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		return s.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("aosc_api"), jwt.WithExpirationRequired())
	if err != nil {
		return nil, ErrTokenInvalido
	}

	userID, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || c.ID == "" {
		return nil, ErrTokenInvalido
	}

	var revocado int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM app_tokens_revocados WHERE jti = ?", c.ID).Scan(&revocado); err != nil {
		return nil, err
	}
	if revocado > 0 {
		return nil, ErrTokenInvalido
	}

	u, err := s.BuscarUsuario(ctx, "id", userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrTokenInvalido
	}

	return &Sesion{Usuario: *u, JTI: c.ID, ExpiraEn: c.ExpiresAt.Time}, nil
}

// Revocar invalida el token de la sesión (logout).
func (s *Servicio) Revocar(ctx context.Context, sesion *Sesion) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT IGNORE INTO app_tokens_revocados (jti, users_id, expira_en) VALUES (?, ?, ?)",
		sesion.JTI, sesion.Usuario.ID, sesion.ExpiraEn.UTC().Format("2006-01-02 15:04:05"))
	return err
}

// LimpiarRevocados borra los tokens revocados que ya vencieron solos.
func (s *Servicio) LimpiarRevocados(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM app_tokens_revocados WHERE expira_en < UTC_TIMESTAMP()")
	return err
}

// BuscarUsuario por "id" o "email". Devuelve nil si no existe.
func (s *Servicio) BuscarUsuario(ctx context.Context, campo string, valor any) (*Usuario, error) {
	if campo != "id" && campo != "email" {
		return nil, fmt.Errorf("campo no permitido: %s", campo)
	}
	var u Usuario
	var name, email sql.NullString
	var activo sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT id, name, email, activo FROM users WHERE "+campo+" = ? ORDER BY id LIMIT 1", valor).
		Scan(&u.ID, &name, &email, &activo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Name, u.Email, u.Activo = name.String, email.String, activo.Int64 != 0
	return &u, nil
}

// VerificarPassword compara contra el hash bcrypt de Laravel ($2y$).
func (s *Servicio) VerificarPassword(ctx context.Context, userID int64, password string) (bool, error) {
	var hash sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT password FROM users WHERE id = ?", userID).Scan(&hash); err != nil {
		return false, err
	}
	if !hash.Valid {
		return false, nil
	}
	return bcrypt.CompareHashAndPassword([]byte(hash.String), []byte(password)) == nil, nil
}

// HashPassword genera un hash bcrypt compatible con Hash::check de Laravel.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	return string(h), err
}

// TienePermiso replica User::tiene_permiso($slug) (roles → permisos).
func (s *Servicio) TienePermiso(ctx context.Context, userID int64, slug string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM role_user ru
		JOIN permission_role pr ON pr.role_id = ru.role_id
		JOIN permissions p ON p.id = pr.permission_id
		WHERE ru.user_id = ? AND p.slug = ?`, strconv.FormatInt(userID, 10), slug).Scan(&n)
	return n > 0, err
}
