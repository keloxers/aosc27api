// Package pdf arma el PDF de una factura sobre la plantilla activa
// (configuracions.factura_plantilla) con el QR de Wee: es el port de
// App\Services\FacturaPdfService, con las mismas posiciones y textos.
package pdf

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-pdf/fpdf"
	qrcode "github.com/skip2/go-qrcode"

	"aosc_api/internal/facturacion"
	"aosc_api/internal/wee"
)

type Generador struct {
	db          *sql.DB
	cobros      *wee.Cobros
	archivosURL string
	http        *http.Client

	mu        sync.Mutex
	plantilla map[string][]byte
}

func NuevoGenerador(db *sql.DB, cobros *wee.Cobros, archivosURL string) *Generador {
	return &Generador{
		db:          db,
		cobros:      cobros,
		archivosURL: archivosURL,
		http:        &http.Client{Timeout: 20 * time.Second},
		plantilla:   map[string][]byte{},
	}
}

func NombreArchivo(m *facturacion.Movimiento) string {
	return "factura-" + numFactura(m) + ".pdf"
}

func numFactura(m *facturacion.Movimiento) string {
	return facturacion.RellenarIzq(strconv.FormatInt(m.ID, 10), 18, "0")
}

type titular struct {
	ID             int64
	Nombre         string
	Email          string
	Cuit           string
	IvaTipo        sql.NullString
	ComprobantesID sql.NullInt64
	Alicuota       sql.NullString
	Categoria      sql.NullString
}

type medicion struct {
	ID               int64
	InmueblesID      sql.NullInt64
	MedicionAnterior sql.NullInt64
	Medicion         sql.NullInt64
	Consumo          sql.NullInt64
	FechaPlanilla    sql.NullTime
}

// Generar devuelve el contenido del PDF.
func (g *Generador) Generar(ctx context.Context, m *facturacion.Movimiento) ([]byte, error) {
	calc := facturacion.NuevaCalculadora(ctx, g.db)

	// Corrección heredada de Laravel: si los importes quedaron cargados con
	// IVA incluido se lo quitan y se guarda. El porcentaje es siempre 21%
	// porque allá la relación (`ivastipos`) no existe y cae en el catch.
	if math.Abs((m.CargoFijo+m.ImporteConsumo+m.ImporteCloaca)-m.ImporteApagar.Float64) < 0.1 {
		m.CargoFijo /= 1.21
		m.ImporteConsumo /= 1.21
		m.ImporteCloaca /= 1.21
		if _, err := g.db.ExecContext(ctx,
			"UPDATE movimientos SET cargo_fijo = ?, importe_consumo = ?, importe_cloaca = ?, updated_at = ? WHERE id = ?",
			m.CargoFijo, m.ImporteConsumo, m.ImporteCloaca, calc.Ahora.Format("2006-01-02 15:04:05"), m.ID); err != nil {
			return nil, err
		}
	}

	inm, err := calc.Inmueble(m.InmueblesID)
	if err != nil {
		return nil, err
	}
	if inm == nil {
		return nil, fmt.Errorf("la factura %d no tiene inmueble", m.ID)
	}

	tit, err := g.titular(ctx, inm)
	if err != nil {
		return nil, err
	}

	importeConsumo := m.ImporteConsumo
	if importeConsumo <= 0 {
		importeConsumo = m.ImporteApagar.Float64 - m.CargoFijo - m.ImporteCloaca - m.ImporteIva.Float64
	}

	precioM3 := ""
	if m.Consumo.Int64 > 0 && importeConsumo > 0 {
		precioM3 = "$" + facturacion.NumberFormat(importeConsumo/float64(m.Consumo.Int64), 2, ".", ",")
	} else if inm.PreciariosID.Valid {
		var tipo sql.NullString
		err := g.db.QueryRowContext(ctx, "SELECT tipo FROM preciarios WHERE id = ?", inm.PreciariosID.Int64).Scan(&tipo)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		// En Laravel la rama "m3" llama a una relación inexistente y termina
		// imprimiendo un espacio.
		if tipo.String == "m3" {
			precioM3 = " "
		}
	}

	barrio := g.nombre(ctx, "SELECT barrio FROM barrios WHERE id = ?", inm.BarriosID)
	calle := g.nombre(ctx, "SELECT calle FROM calles WHERE id = ?", inm.CallesID)
	distrito := g.nombre(ctx, "SELECT distrito FROM distritos WHERE id = ?", inm.DistritosID)
	medidor := g.nombre(ctx, "SELECT numero FROM medidors WHERE id = ?", inm.MedidorsID)

	direccion := ""
	if verdadero(barrio) {
		direccion += barrio
	}
	if verdadero(calle) {
		direccion += " - " + calle
	}
	if verdadero(inm.Altura.String) {
		direccion += " n° " + inm.Altura.String
	}
	if verdadero(inm.Mz.String) {
		direccion += " mz: " + inm.Mz.String
	}
	if verdadero(inm.Departamento.String) {
		direccion += " dpto/casa: " + inm.Departamento.String
	}
	if verdadero(inm.Piso.String) {
		direccion += " piso: " + inm.Piso.String
	}

	discrimina := tit != nil && tit.ComprobantesID.Valid && tit.ComprobantesID.Int64 == 1

	var med *medicion
	if m.MedicionsID.Valid {
		if med, err = g.medicion(ctx, "m.id = ?", m.MedicionsID.Int64); err != nil {
			return nil, err
		}
	}
	ultima, err := g.medicion(ctx, "m.inmuebles_id = ? ORDER BY m.id DESC", inm.ID)
	if err != nil {
		return nil, err
	}

	var fechaUltimaLectura string
	switch {
	case m.FechaLecturaAnterior.Valid:
		fechaUltimaLectura = m.FechaLecturaAnterior.Time.Format("02-01-2006")
	case m.MedicionsID.Valid:
		if med != nil {
			anterior, err := g.medicion(ctx, "m.id < ? AND m.inmuebles_id = ? ORDER BY m.id DESC", med.ID, med.InmueblesID)
			if err != nil {
				return nil, err
			}
			if anterior != nil && anterior.FechaPlanilla.Valid {
				fechaUltimaLectura = anterior.FechaPlanilla.Time.Format("02/01/2006")
			}
		}
	default:
		if ultima != nil && ultima.FechaPlanilla.Valid {
			fechaUltimaLectura = ultima.FechaPlanilla.Time.Format("02/01/2006")
		}
	}

	var ultimaLectura string
	switch {
	case m.LecturaAnterior.Valid:
		ultimaLectura = fmt.Sprintf("%d m³", m.LecturaAnterior.Int64)
	case m.MedicionsID.Valid:
		ultimaLectura = entero(med, func(x *medicion) sql.NullInt64 { return x.MedicionAnterior }) + " m³"
	default:
		ultimaLectura = entero(ultima, func(x *medicion) sql.NullInt64 { return x.MedicionAnterior }) + " m³"
	}

	var fechaLecturaActual string
	switch {
	case m.FechaLecturaActual.Valid:
		fechaLecturaActual = m.FechaLecturaActual.Time.Format("02-01-2006")
	case m.MedicionsID.Valid:
		if med != nil && med.FechaPlanilla.Valid {
			fechaLecturaActual = med.FechaPlanilla.Time.Format("02/01/2006")
		}
	default:
		fechaLecturaActual = fecha(m.CreatedAt, "02/01/2006")
	}

	var lecturaActual string
	switch {
	case m.LecturaActual.Valid:
		lecturaActual = fmt.Sprintf("%d m³", m.LecturaActual.Int64)
	case m.MedicionsID.Valid:
		lecturaActual = entero(med, func(x *medicion) sql.NullInt64 { return x.Medicion }) + " m³"
	default:
		anterior := int64(0)
		if ultima != nil {
			anterior = ultima.MedicionAnterior.Int64
		}
		lecturaActual = fmt.Sprintf("%d m³", anterior+m.Consumo.Int64)
	}

	interesImporte, err := calc.Intereses(m)
	if err != nil {
		return nil, err
	}

	porcentajeIvaTexto := ""
	porcentajeIva := 0.0
	if !discrimina {
		porcentajeIvaTexto = "21%"
		if tit != nil && tit.Alicuota.Valid {
			porcentajeIvaTexto = tit.Alicuota.String
		}
		porcentajeIva, _ = strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(porcentajeIvaTexto, "%", "")), 64)

		if m.CargoFijo > 0 {
			m.CargoFijo += m.CargoFijo * porcentajeIva / 100
		}
		if m.ImporteConsumo > 0 {
			m.ImporteConsumo += m.ImporteConsumo * porcentajeIva / 100
		}
		if m.ImporteCloaca > 0 {
			m.ImporteCloaca += m.ImporteCloaca * porcentajeIva / 100
		}
		if interesImporte != 0 {
			interesImporte += interesImporte * porcentajeIva / 100
		}
	}

	d := nuevoDocumento()

	plantilla, err := g.nombrePlantilla(ctx)
	if err != nil {
		return nil, err
	}
	if img := g.imagenPlantilla(ctx, plantilla); img != nil {
		d.imagen(plantilla, img)
	}

	d.tamano(10)
	if inm.CodigoCodex.String != "" {
		d.texto(96, 49, "CODIGO: "+inm.CodigoCodex.String)
	}
	if verdadero(inm.Cuenta.String) {
		d.texto(120, 54.5, inm.Cuenta.String)
	}
	if tit != nil {
		d.texto(117, 61, tit.Nombre)
		d.texto(134, 68, tit.IvaTipo.String)
		if verdadero(tit.Cuit) {
			d.texto(117, 80, tit.Cuit)
		}
	}
	d.tamano(8)
	d.texto(120, 85.7, direccion)
	d.tamano(10)
	d.texto(120, 90, distrito)
	if tit != nil {
		d.texto(141, 94.5, tit.Categoria.String)
	}

	d.texto(115, 104.5, medidor)
	d.texto(135, 108.5, fechaUltimaLectura)
	d.texto(126, 113, ultimaLectura)
	d.texto(133, 117, fechaLecturaActual)
	d.texto(124, 121.5, lecturaActual)
	if m.MedicionsID.Valid {
		d.texto(137, 125.5, entero(med, func(x *medicion) sql.NullInt64 { return x.Consumo })+" m³")
	} else {
		d.texto(137, 125.5, entero0(m.Consumo)+" m³")
	}
	d.texto(158, 134, precioM3)

	num := numFactura(m)
	d.texto(122, 146, num)
	d.texto(129, 151, fecha(m.CreatedAt, "02/01/2006"))

	if m.PlanspagosID.Valid {
		d.texto(115, 156, m.Movimiento.String)
	} else if m.PeriodoMes.Int64 != 0 && m.PeriodoAnio.Int64 != 0 {
		d.texto(115, 156, fmt.Sprintf("%d / %d", m.PeriodoMes.Int64, m.PeriodoAnio.Int64))
	}

	y := 166.0
	linea := func(x float64, s string) {
		d.texto(x, y, s)
		y += 4
	}

	if inm.TipoServicio == "Medido" {
		linea(96, "CARGO FIJO: "+facturacion.Moneda(m.CargoFijo))
		d.texto(96, y, "CONSUMO: ")
		switch {
		case m.ImporteConsumo > 0:
			linea(116, facturacion.Moneda(m.ImporteConsumo))
		case !m.MedicionsID.Valid:
			linea(116, facturacion.Moneda(m.ImporteApagar.Float64))
		default:
			// Laravel pasa mal los argumentos de number_format: sin decimales.
			linea(116, "$"+facturacion.NumberFormat(m.ImporteApagar.Float64-m.CargoFijo-m.ImporteCloaca-m.ImporteIva.Float64, 0, ".", ","))
		}
		d.texto(96, y, "CLOACA: ")
		linea(113, facturacion.Moneda(m.ImporteCloaca))
	} else {
		d.texto(96, y, "CONSUMO: ")
		var importe sql.NullFloat64
		if discrimina || porcentajeIvaTexto == "0%" {
			importe = m.ImporteBruto
		} else {
			importe = m.ImporteApagar
		}
		if importe.Valid {
			d.texto(116, y, facturacion.Moneda(importe.Float64))
		}
		y += 4
	}

	descuentos, nombresDescuentos, err := calc.Descuentos(m)
	if err != nil {
		return nil, err
	}
	recargos, nombresRecargos, err := calc.Recargos(m)
	if err != nil {
		return nil, err
	}

	if recargos > 0 {
		nombre := strings.ToUpper(g.nombre(ctx, "SELECT descuento FROM descuentos WHERE id = ?", inm.DescuentosID))
		linea(96, nombre+": "+facturacion.Moneda(recargos))
	}

	fechaInteres := calc.Ahora.Format("02/01/2006")
	if m.FechaPago.Valid {
		fechaInteres = m.FechaPago.Time.Format("02/01/2006")
	} else if f, err := g.fechaPago(ctx, m); err != nil {
		return nil, err
	} else if f.Valid {
		fechaInteres = f.Time.Format("02/01/2006")
	}

	if interesImporte > 0 {
		d.texto(96, y, "INTERESES AL ")
		d.texto(122, y, fechaInteres+": ")
		linea(142, facturacion.Moneda(interesImporte))
	}

	conIva := func(x float64) float64 {
		if porcentajeIva > 0 {
			return x + x*porcentajeIva/100
		}
		return x
	}

	if descuentos > 0 && m.AplicaDescuento {
		linea(96, "OTROS CONCEPTOS:")
		for _, n := range nombresDescuentos {
			linea(100, "- "+n)
		}
		linea(96, "TOTAL OTROS CONCEPTOS: -"+facturacion.Moneda(conIva(descuentos)))
	}

	if recargos > 0 {
		linea(96, "OTROS CONCEPTOS:")
		for _, n := range nombresRecargos {
			linea(100, "- "+n)
		}
		linea(96, "TOTAL OTROS CONCEPTOS: "+facturacion.Moneda(conIva(recargos)))
	}

	conceptos, err := calc.Conceptos(m)
	if err != nil {
		return nil, err
	}
	for _, c := range conceptos {
		linea(96, strings.ToUpper(c.Nombre)+": "+facturacion.Moneda(c.Importe))
	}

	if discrimina {
		iva, err := calc.ImporteIva(m)
		if err != nil {
			return nil, err
		}
		linea(96, "IVA: "+facturacion.Moneda(iva))
	}

	// Comprobantes adeudados: en Laravel la relación no existe y siempre da 0.
	d.tamano(8)
	d.texto(20, 196.5, facturacion.NumberFormat(0, 2, ",", "."))

	d.negrita(10)
	d.texto(144, 221, num)
	d.texto(144, 226, fecha(m.CreatedAt, "02/01/2006"))
	consumoMedicion := "0"
	if med != nil {
		consumoMedicion = entero0(med.Consumo)
	}
	d.texto(144, 231.5, facturacion.RellenarIzq(consumoMedicion, 13, "0"))
	d.texto(144, 236, fechaOEpoch(m.Fecha1erVencimiento))
	d.texto(78, 241.5, "INTERESES AL "+fechaInteres)
	d.texto(144, 241.5, facturacion.Moneda(interesImporte))

	final, err := calc.ImporteFinal(m)
	if err != nil {
		return nil, err
	}
	d.texto(144, 246.5, facturacion.Moneda(final))
	d.texto(7, 255, "CUENTA N°: "+inm.Cuenta.String)

	var pagador *wee.Pagador
	if tit != nil {
		pagador = &wee.Pagador{ID: tit.ID, Nombre: tit.Nombre, Email: tit.Email}
	}
	if urlQR, checkoutURL, ok := g.cobros.QR(ctx, calc, m, inm, pagador); ok {
		if err := d.qr(urlQR, 86, 255, 40); err != nil {
			return nil, err
		}
		d.negrita(7)
		d.texto(87.5, 254.5, "QR valido hasta el "+fechaOEpoch(m.Fecha1erVencimiento))
		d.texto(60, 292.5, checkoutURL)
	}

	return d.salida()
}

func (g *Generador) titular(ctx context.Context, inm *facturacion.Inmueble) (*titular, error) {
	if !inm.UsersID.Valid {
		return nil, nil
	}
	var t titular
	var nombre, email, cuit sql.NullString
	err := g.db.QueryRowContext(ctx, "SELECT u.id, u.name, u.email, u.cuit, it.ivastipo, it.comprobantes_id, a.`desc`, uc.userscategoria"+
		" FROM users u"+
		" LEFT JOIN ivastipos it ON it.id = u.ivastipos_id"+
		" LEFT JOIN `alícuotas_disponibles` a ON a.id = it.alicuotas_id"+
		" LEFT JOIN userscategorias uc ON uc.id = u.userscategorias_id"+
		" WHERE u.id = ?", inm.UsersID.Int64).
		Scan(&t.ID, &nombre, &email, &cuit, &t.IvaTipo, &t.ComprobantesID, &t.Alicuota, &t.Categoria)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.Nombre, t.Email, t.Cuit = nombre.String, email.String, cuit.String
	return &t, nil
}

func (g *Generador) medicion(ctx context.Context, where string, args ...any) (*medicion, error) {
	var md medicion
	err := g.db.QueryRowContext(ctx, `
		SELECT m.id, m.inmuebles_id, m.medicion_anterior, m.medicion, m.consumo, mp.fecha
		FROM medicions m
		LEFT JOIN medicionsplanillas mp ON mp.id = m.medicionsplanillas_id
		WHERE `+where+` LIMIT 1`, args...).
		Scan(&md.ID, &md.InmueblesID, &md.MedicionAnterior, &md.Medicion, &md.Consumo, &md.FechaPlanilla)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &md, err
}

// fechaPago: fecha del recibo cerrado que pagó la factura, sea por Wee
// (pago completado) o por una caja que no es la de Wee (Movimiento::fechaPago()).
func (g *Generador) fechaPago(ctx context.Context, m *facturacion.Movimiento) (sql.NullTime, error) {
	var f sql.NullTime
	err := g.db.QueryRowContext(ctx, `
		SELECT cr.fecha
		FROM cajasrecibosdetalles crd
		JOIN cajasrecibos cr ON cr.id = crd.cajasrecibos_id
		WHERE crd.movimientos_id = ? AND crd.importe_pagado != 0 AND cr.estado = 'cerrada'
		  AND (EXISTS (SELECT 1 FROM payment_intent pi WHERE pi.cajasrecibos_id = cr.id AND pi.transaccion = 'completado')
		       OR EXISTS (SELECT 1 FROM cajasplanillas cp WHERE cp.id = cr.cajasplanillas_id
		                  AND cp.users_id != (SELECT numero FROM configuracions WHERE configuracion = 'wee_user_id' LIMIT 1)))
		ORDER BY crd.id LIMIT 1`, m.ID).Scan(&f)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	return f, err
}

func (g *Generador) nombre(ctx context.Context, consulta string, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	var s sql.NullString
	_ = g.db.QueryRowContext(ctx, consulta, id.Int64).Scan(&s)
	return s.String
}

func (g *Generador) nombrePlantilla(ctx context.Context) (string, error) {
	var archivo sql.NullString
	err := g.db.QueryRowContext(ctx,
		"SELECT archivo FROM configuracions WHERE configuracion = 'factura_plantilla' AND estado = 1 ORDER BY id LIMIT 1").Scan(&archivo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return archivo.String, err
}

// imagenPlantilla descarga (y cachea) el fondo de la factura desde MinIO.
// Si no se puede obtener, la factura sale sin fondo, como en Laravel cuando
// el archivo no existe.
func (g *Generador) imagenPlantilla(ctx context.Context, archivo string) []byte {
	if archivo == "" || g.archivosURL == "" {
		return nil
	}
	g.mu.Lock()
	img, ok := g.plantilla[archivo]
	g.mu.Unlock()
	if ok {
		return img
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.archivosURL+"/"+archivo, nil)
	if err != nil {
		return nil
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	img, err = io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil
	}

	g.mu.Lock()
	g.plantilla[archivo] = img
	g.mu.Unlock()
	return img
}

// verdadero: un string es "truthy" en PHP salvo "" y "0".
func verdadero(s string) bool {
	return s != "" && s != "0"
}

func fecha(t sql.NullTime, formato string) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format(formato)
}

// fechaOEpoch replica date('d/m/Y', strtotime(null)).
func fechaOEpoch(t sql.NullTime) string {
	if !t.Valid {
		return "01/01/1970"
	}
	return t.Time.Format("02/01/2006")
}

func entero(md *medicion, campo func(*medicion) sql.NullInt64) string {
	if md == nil {
		return ""
	}
	v := campo(md)
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

func entero0(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

// documento envuelve fpdf imitando el posicionamiento de TCPDF::Text().
type documento struct {
	pdf *fpdf.Fpdf
	tr  func(string) string
	// Línea base relativa al tamaño de letra: TCPDF centra el texto en una
	// celda de 1.25 × tamaño, (1.25 + ascent − descent) / 2 de Helvetica.
	base float64
}

// Padding izquierdo de celda por defecto de TCPDF (1 cm / 10).
const paddingCelda = 1.000125

func nuevoDocumento() *documento {
	p := fpdf.New("P", "mm", "A4", "")
	p.SetAutoPageBreak(false, 0)
	p.SetMargins(0, 0, 0)
	p.SetTitle("Factura", true)
	p.AddPage()
	d := &documento{pdf: p, tr: p.UnicodeTranslatorFromDescriptor("")}
	d.tamano(10)
	return d
}

func (d *documento) tamano(pt float64) {
	d.pdf.SetFont("Helvetica", "", pt)
	d.base = (1.25 + 0.931 - 0.225) / 2
}

func (d *documento) negrita(pt float64) {
	d.pdf.SetFont("Helvetica", "B", pt)
	d.base = (1.25 + 0.962 - 0.228) / 2
}

func (d *documento) texto(x, y float64, s string) {
	if s == "" {
		return
	}
	_, mm := d.pdf.GetFontSize()
	d.pdf.Text(x+paddingCelda, y+d.base*mm, d.tr(s))
}

func (d *documento) imagen(nombre string, img []byte) {
	tipo := strings.TrimPrefix(strings.ToLower(path.Ext(nombre)), ".")
	if tipo == "jpeg" {
		tipo = "jpg"
	}
	opt := fpdf.ImageOptions{ImageType: tipo, ReadDpi: false}
	d.pdf.RegisterImageOptionsReader("plantilla", opt, bytes.NewReader(img))
	if d.pdf.Err() {
		// Una plantilla ilegible no impide emitir la factura.
		d.pdf.ClearError()
		return
	}
	w, h := d.pdf.GetPageSize()
	d.pdf.ImageOptions("plantilla", 0, 0, w, h, false, opt, 0, "")
}

func (d *documento) qr(contenido string, x, y, lado float64) error {
	png, err := qrcode.Encode(contenido, qrcode.Low, 512)
	if err != nil {
		return err
	}
	opt := fpdf.ImageOptions{ImageType: "png"}
	d.pdf.RegisterImageOptionsReader("qr", opt, bytes.NewReader(png))
	d.pdf.ImageOptions("qr", x, y, lado, lado, false, opt, 0, "")
	return nil
}

func (d *documento) salida() ([]byte, error) {
	var buf bytes.Buffer
	if err := d.pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
