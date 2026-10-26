// Package wee habla con el gateway de cobros Wee: checkout de varias
// facturas (WeeCheckoutService) y QR de una factura para el PDF
// (WeeController::WeeQr). La confirmación del pago sigue llegando por el
// webhook /wee/webhook de aosc_front.
package wee

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

type Item struct {
	Quantity    int    `json:"quantity"`
	Description string `json:"description"`
	Identifier  string `json:"identifier"`
	Amount      int64  `json:"amount"`
}

type Pedido struct {
	PayerName         string `json:"payer_name"`
	Description       string `json:"description"`
	FirstTotal        int64  `json:"first_total"`
	FirstDueDate      string `json:"first_due_date"`
	ExternalReference string `json:"external_reference"`
	PayerEmail        string `json:"payer_email"`
	PaymentType       string `json:"payment_type"`
	Items             []Item `json:"items"`
}

type Cliente struct {
	http *http.Client
}

func NuevoCliente() *Cliente {
	return &Cliente{http: &http.Client{Timeout: 30 * time.Second}}
}

// Checkout crea un checkout y devuelve su uuid y la URL de pago.
func (c *Cliente) Checkout(ctx context.Context, endpoint, apikey string, p Pedido) (uuid, checkoutURL string, err error) {
	var r struct {
		Data struct {
			UUID        string `json:"uuid"`
			CheckoutURL string `json:"checkout_url"`
		} `json:"data"`
	}
	if err := c.post(ctx, endpoint+"/checkout", apikey, p, &r); err != nil {
		return "", "", err
	}
	if r.Data.UUID == "" || r.Data.CheckoutURL == "" {
		return "", "", fmt.Errorf("respuesta de Wee sin uuid/checkout_url")
	}
	return r.Data.UUID, r.Data.CheckoutURL, nil
}

// QRFactura crea el QR de pago de una factura (el body es una lista).
func (c *Cliente) QRFactura(ctx context.Context, endpoint, apikey string, p Pedido) (uuid, urlQR, checkoutURL string, err error) {
	var r struct {
		Data []struct {
			UUID        string `json:"uuid"`
			URLQR       string `json:"url_qr"`
			CheckoutURL string `json:"checkout_url"`
		} `json:"data"`
	}
	if err := c.post(ctx, endpoint+"/checkout/qr-factura", apikey, []Pedido{p}, &r); err != nil {
		return "", "", "", err
	}
	if len(r.Data) == 0 {
		return "", "", "", fmt.Errorf("respuesta de Wee sin datos de QR")
	}
	return r.Data[0].UUID, r.Data[0].URLQR, r.Data[0].CheckoutURL, nil
}

func (c *Cliente) post(ctx context.Context, url, apikey string, cuerpo, destino any) error {
	b, err := json.Marshal(cuerpo)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("apiKey", apikey)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Wee respondió %d: %s", resp.StatusCode, body)
	}
	return json.Unmarshal(body, destino)
}

const alfabeto = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// CodigoAleatorio equivale a Str::random(n).
func CodigoAleatorio(n int) string {
	b := make([]byte, n)
	max := big.NewInt(int64(len(alfabeto)))
	for i := range b {
		k, _ := rand.Int(rand.Reader, max)
		b[i] = alfabeto[k.Int64()]
	}
	return string(b)
}
