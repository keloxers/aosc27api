// Package mail envía correos por SMTP con la misma cuenta que usa Laravel
// (variables MAIL_* de aosc_front).
package mail

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"html"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"aosc_api/internal/config"
)

type Mailer struct {
	cfg *config.Config
}

func NuevoMailer(cfg *config.Config) *Mailer {
	return &Mailer{cfg: cfg}
}

// Habilitado indica si hay un servidor SMTP configurado.
func (m *Mailer) Habilitado() bool {
	return m.cfg.MailHost != ""
}

// EnviarValidacion manda el correo de validación de cuenta (misma vista que
// resources/views/emails/validar.blade.php).
func (m *Mailer) EnviarValidacion(para, nombre string, codigo int) error {
	link := fmt.Sprintf("%s/%d", m.cfg.ValidarCorreoURL, codigo)
	cuerpo := `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Valida tu correo electrónico</title>
</head>
<body>
    <h1>Hola ` + html.EscapeString(nombre) + `,</h1>
    <p>Por favor, valida tu correo electrónico ingresando el siguiente enlace:</p>
    <p><a href="` + link + `">` + link + `</a></p>
    <p>Si no has solicitado este correo electrónico, por favor ignóralo.</p>
</body>
</html>`
	return m.Enviar(para, "Valida tu correo electrónico", cuerpo)
}

// Enviar manda un correo HTML.
func (m *Mailer) Enviar(para, asunto, cuerpoHTML string) error {
	if !m.Habilitado() {
		return fmt.Errorf("MAIL_HOST no configurado")
	}

	from := m.cfg.MailFrom
	var msg strings.Builder
	msg.WriteString("From: " + mime.QEncoding.Encode("utf-8", m.cfg.MailFromName) + " <" + from + ">\r\n")
	msg.WriteString("To: " + para + "\r\n")
	msg.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", asunto) + "\r\n")
	msg.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	msg.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	b64 := base64.StdEncoding.EncodeToString([]byte(cuerpoHTML))
	for len(b64) > 76 {
		msg.WriteString(b64[:76] + "\r\n")
		b64 = b64[76:]
	}
	msg.WriteString(b64 + "\r\n")

	addr := net.JoinHostPort(m.cfg.MailHost, m.cfg.MailPort)
	tlsCfg := &tls.Config{ServerName: m.cfg.MailHost}

	var c *smtp.Client
	var err error
	if m.cfg.MailEncryption == "ssl" {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, tlsCfg)
		if err != nil {
			return err
		}
		c, err = smtp.NewClient(conn, m.cfg.MailHost)
		if err != nil {
			return err
		}
	} else {
		conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
		if err != nil {
			return err
		}
		c, err = smtp.NewClient(conn, m.cfg.MailHost)
		if err != nil {
			return err
		}
		if m.cfg.MailEncryption == "tls" {
			if err = c.StartTLS(tlsCfg); err != nil {
				c.Close()
				return err
			}
		}
	}
	defer c.Close()

	if m.cfg.MailUsername != "" {
		if err = c.Auth(smtp.PlainAuth("", m.cfg.MailUsername, m.cfg.MailPassword, m.cfg.MailHost)); err != nil {
			return err
		}
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	if err = c.Rcpt(para); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write([]byte(msg.String())); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
