<?php

/**
 * Genera los importes calculados por aosc_front (App\Models\Movimiento e
 * Inmueble) para una muestra de facturas, para compararlos con el port en Go:
 *
 *   php herramientas/referencia_laravel.php /ruta/a/aosc_front > referencia.json
 *   LARAVEL_REFERENCIA=referencia.json go test ./internal/facturacion -run Laravel -v
 *
 * Correr los dos el mismo día: intereses y estados dependen de la fecha.
 */

$front = $argv[1] ?? __DIR__ . '/../../aosc_front';
require $front . '/vendor/autoload.php';
$app = require $front . '/bootstrap/app.php';
$app->make(Illuminate\Contracts\Console\Kernel::class)->bootstrap();

use App\Models\Inmueble;
use App\Models\Movimiento;
use Illuminate\Support\Facades\DB;

$ids = fn (string $sql) => array_map(fn ($r) => (int) $r->id, DB::select($sql));

$movimientos = array_unique(array_merge(
    $ids("SELECT id FROM movimientos WHERE (importe_pagado <= 0 OR importe_pagado IS NULL) AND importe_apagar > 0 ORDER BY RAND() LIMIT 1500"),
    $ids("SELECT id FROM movimientos WHERE importe_pagado > 0 ORDER BY RAND() LIMIT 300"),
    $ids("SELECT m.id FROM movimientos m JOIN inmuebles_descuentos d ON d.inmuebles_id = m.inmuebles_id ORDER BY RAND() LIMIT 400"),
    $ids("SELECT DISTINCT movimientos_id AS id FROM movimientosconceptosfijos ORDER BY RAND() LIMIT 300"),
    $ids("SELECT m.id FROM movimientos m JOIN inmuebles i ON i.id = m.inmuebles_id WHERE i.exencions_id IS NOT NULL ORDER BY RAND() LIMIT 200"),
    $ids("SELECT m.id FROM movimientos m JOIN inmuebles i ON i.id = m.inmuebles_id JOIN tarifasespecials t ON t.users_id = i.users_id ORDER BY RAND() LIMIT 200"),
    $ids("SELECT DISTINCT movimientos_id AS id FROM cajasrecibosdetalles WHERE importe_pagado > 0 ORDER BY RAND() LIMIT 200"),
));

$salida = ['generado' => date('c'), 'movimientos' => [], 'inmuebles' => []];

foreach (Movimiento::whereIn('id', $movimientos)->get() as $m) {
    if (!$m->inmuebles) {
        continue;
    }
    $descuentos = $m->descuentos();
    $recargos = $m->recargos();
    $salida['movimientos'][] = [
        'id' => $m->id,
        'importe_final' => (float) $m->importeFinal(),
        'intereses' => (float) $m->intereses(),
        'descuento' => (float) $descuentos['descuento'],
        'descuentos' => $descuentos['descuentos'],
        'recargo' => (float) $recargos['recargo'],
        'recargos' => $recargos['recargos'],
        'importe_iva' => (float) $m->importeIva(),
        'conceptos_total' => (float) $m->conceptosTotal(),
        'estado' => $m->estadoLabel(),
    ];
}

$inmuebles = $ids("SELECT DISTINCT inmuebles_id AS id FROM movimientos WHERE (importe_pagado <= 0 OR importe_pagado IS NULL) AND importe_apagar > 0 ORDER BY RAND() LIMIT 300");
foreach (Inmueble::whereIn('id', $inmuebles)->get() as $i) {
    $salida['inmuebles'][] = [
        'id' => $i->id,
        'total_pendiente' => $i->totalPendiente(),
        'cantidad_pendientes' => $i->movimientosPendientes()->count(),
        'estado' => $i->estadoLabel(),
        'direccion' => $i->direccionCorta(),
    ];
}

echo json_encode($salida, JSON_UNESCAPED_UNICODE | JSON_PRESERVE_ZERO_FRACTION);
