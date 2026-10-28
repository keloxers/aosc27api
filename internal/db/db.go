// Package db abre la conexión a la base MySQL/MariaDB `aosc`, la misma que
// usan aosc_front y aoscback.
package db

import (
	"database/sql"
	"fmt"
	"time"
	_ "time/tzdata" // la imagen Docker no trae zonas horarias

	"github.com/go-sql-driver/mysql"

	"aosc_api/internal/config"
)

// Zona es la zona horaria de la base y de toda la lógica de negocio.
var Zona = func() *time.Location {
	loc, err := time.LoadLocation("America/Argentina/Buenos_Aires")
	if err != nil {
		return time.FixedZone("ART", -3*3600)
	}
	return loc
}()

func Abrir(c *config.Config) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = c.DBUsername
	cfg.Passwd = c.DBPassword
	cfg.Net = "tcp"
	cfg.Addr = c.DBHost + ":" + c.DBPort
	cfg.DBName = c.DBDatabase
	// La base (MariaDB con time_zone -03:00) y Laravel (timezone America/Argentina/Buenos_Aires)
	// guardan las fechas en hora de Argentina; los DATETIME sin zona se leen y escriben en esa zona.
	cfg.ParseTime = true
	cfg.Loc = Zona
	cfg.Params = map[string]string{"charset": "utf8mb4"}

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("no se pudo conectar a la base: %w", err)
	}

	return db, nil
}

// Ahora devuelve la fecha y hora actual en el formato de los timestamps de
// Laravel (created_at/updated_at).
func Ahora() string {
	return AhoraAR().Format("2006-01-02 15:04:05")
}

// AhoraAR devuelve la hora actual en la zona de Argentina.
func AhoraAR() time.Time {
	return time.Now().In(Zona)
}

// HoyAR devuelve la fecha de hoy (Y-m-d) en la zona de Argentina.
func HoyAR() string {
	return AhoraAR().Format("2006-01-02")
}
