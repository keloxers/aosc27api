// aosc-api: API de la app móvil de AOSC, independiente de aosc_front.
//
// Uso:
//
//	aosc-api                                  servidor HTTP (+ push si está habilitado)
//	aosc-api notificaciones [-simular] [-regla N]  una corrida manual de push
//	aosc-api migrar                           crea las tablas propias del servicio
//	aosc-api healthcheck                      sale con 0 si el servidor responde
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aosc_api/internal/api"
	"aosc_api/internal/auth"
	"aosc_api/internal/config"
	"aosc_api/internal/db"
	"aosc_api/internal/fcm"
	"aosc_api/internal/mail"
	"aosc_api/internal/notificaciones"
	"aosc_api/internal/pdf"
	"aosc_api/internal/wee"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// Healthcheck del contenedor (la imagen distroless no trae curl).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	if err := ejecutar(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/up")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func ejecutar() error {
	cfg, err := config.Cargar()
	if err != nil {
		return err
	}
	conexion, err := db.Abrir(cfg)
	if err != nil {
		return err
	}
	defer conexion.Close()

	authSrv := auth.NuevoServicio(conexion, cfg.JWTSecret, cfg.JWTTTL)

	comando := ""
	if len(os.Args) > 1 {
		comando = os.Args[1]
	}

	switch comando {
	case "migrar":
		if _, err := conexion.Exec(auth.MigracionRevocados); err != nil {
			return fmt.Errorf("migración: %w", err)
		}
		slog.Info("migración aplicada")
		return nil

	case "notificaciones":
		fs := flag.NewFlagSet("notificaciones", flag.ExitOnError)
		simular := fs.Bool("simular", false, "mostrar lo que se enviaría sin enviar ni registrar nada")
		regla := fs.Int64("regla", 0, "procesar solo esta regla, sin importar su hora")
		_ = fs.Parse(os.Args[2:])

		var cliente *fcm.Cliente
		if !*simular {
			if cliente, err = fcm.NuevoCliente(cfg.FCMCredenciales); err != nil {
				return err
			}
		}
		_, err := notificaciones.NuevoEnviador(conexion, cliente).Ejecutar(context.Background(), notificaciones.Opciones{
			ReglaID: *regla,
			Simular: *simular,
			Salida:  func(s string) { fmt.Println(s) },
		})
		return err

	case "", "serve":
		return servir(cfg, conexion, authSrv)
	}

	return fmt.Errorf("comando desconocido: %s", comando)
}

func servir(cfg *config.Config, conexion *sql.DB, authSrv *auth.Servicio) error {
	// La tabla de revocados es necesaria para validar tokens.
	if _, err := conexion.Exec(auth.MigracionRevocados); err != nil {
		slog.Warn("no se pudo crear app_tokens_revocados (correr `aosc-api migrar` con un usuario con permisos)", "error", err)
	}

	cobros := wee.NuevoCobros(conexion, cfg.WeeHabilitado)
	srv := api.NuevoServidor(conexion, authSrv, mail.NuevoMailer(cfg), cobros,
		pdf.NuevoGenerador(conexion, cobros, cfg.ArchivosURL))

	ctx, cancelar := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancelar()

	if cfg.NotificacionesHabilitadas {
		cliente, err := fcm.NuevoCliente(cfg.FCMCredenciales)
		if err != nil {
			return err
		}
		go notificaciones.NuevoEnviador(conexion, cliente).Programar(ctx)
		slog.Info("envío de notificaciones push habilitado (cada hora)")
	}

	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				srv.Limitador().Purgar()
				if err := authSrv.LimpiarRevocados(ctx); err != nil {
					slog.Warn("no se pudieron limpiar los tokens revocados", "error", err)
				}
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("escuchando", "addr", cfg.HTTPAddr)
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	apagar, cancelarApagado := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelarApagado()
	return httpSrv.Shutdown(apagar)
}
