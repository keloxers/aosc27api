// Package db abre la conexión a la base MySQL/MariaDB `aosc`, la misma que
// usan aosc_front y aoscback.
package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"aosc_api/internal/config"
)

func Abrir(c *config.Config) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = c.DBUsername
	cfg.Passwd = c.DBPassword
	cfg.Net = "tcp"
	cfg.Addr = c.DBHost + ":" + c.DBPort
	cfg.DBName = c.DBDatabase
	// Laravel guarda las fechas en UTC (config/app.php timezone = UTC).
	cfg.ParseTime = true
	cfg.Loc = time.UTC
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
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}
