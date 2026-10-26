# aosc_api

API en Go para **aosc_app**, independiente de aosc_front. Expone `/api/v1` con
el mismo contrato JSON que la API Sanctum de aosc_front (`routes/api.php`) y
trabaja sobre la misma base `aosc`.

Sirve dos instancias con el mismo código, elegidas con `INSTANCIA`: `aosc` y
`covesa` (COVESA Ituzaingó: base `covesa`, coordenadas propias de la factura,
sin push). Producción: ver [`deploy/README.md`](deploy/README.md).

| Endpoint | Equivale a |
|---|---|
| `POST /register`, `/login`, `/logout`, `GET /me` | `Api\AuthController` |
| `GET/POST /medidores`, `DELETE /medidores/{id}` | `Api\MedidoresController` |
| `GET /medidores/{id}/facturas[/{mov}[/pdf]]`, `/consumos`, `/pagos` | `Api\FacturasController` + `FacturaPdfService` |
| `POST /checkout`, `GET /checkout/{ref}/status` | `WeeCheckoutService` |
| `POST/DELETE /dispositivos` | `Api\DispositivosController` |
| push cada hora (`NOTIFICACIONES_HABILITADAS`) | comando `notificaciones:enviar` |

Lo que **sigue en aosc_front**: el webhook de Wee (`/wee/webhook`), que
confirma los pagos de los checkouts creados acá, y la ruta web
`/validacion/{codigo}` del mail de alta.

## Diferencias con Laravel

- **Tokens JWT propios** (HS256, `JWT_TTL_DIAS`) en lugar de Sanctum. El logout
  revoca el token en la tabla `app_tokens_revocados`. Al pasar a este servicio
  los tokens Sanctum dejan de valer: la app recibe 401, borra el token y pide
  login de nuevo (ya lo maneja `interceptors.dart`).
- Checkout: los ids inválidos o ajenos se ignoran (Laravel cortaba con 500) y
  el filtro de admin agrupa bien el `OR importe_pagado IS NULL`.
- Si el mail de validación falla, el alta responde 201 igual y queda en el log
  (Laravel devolvía 500 con el usuario ya creado).
- Los importes siempre salen como número en el JSON. En algunos casos Laravel
  los devolvía como string.

Los cálculos (`importeFinal`, intereses, descuentos, recargos, IVA, estados)
están portados uno a uno en `internal/facturacion`. **Si cambia una fórmula en
`App\Models\Movimiento`, hay que replicarla acá.** Para verificarlo:

```sh
docker exec -w /var/www/aosc_api laradock-workspace-1 \
  php herramientas/referencia_laravel.php /var/www/aosc_front > referencia.json
LARAVEL_REFERENCIA=referencia.json DB_HOST=127.0.0.1 DB_PASSWORD=... \
  go test ./internal/facturacion -run Laravel -v
```

## Configuración

Copiar `.env.example` a `.env`. Las variables `DB_*`, `MAIL_*` y
`ARCHIVOS_URL` (`MINIO_ENDPOINT/AWS_BUCKET`) salen del `.env` de aosc_front.

```sh
go run ./cmd/aosc-api migrar          # crea app_tokens_revocados (también lo intenta al arrancar)
go run ./cmd/aosc-api                 # servidor
go run ./cmd/aosc-api notificaciones -simular [-regla N]
```

## Pasos para el corte

1. `deploy/docker-compose.yml` en Vi-App-A (ver `deploy/README.md`), con
   `NOTIFICACIONES_HABILITADAS=false`.
2. Probar contra el contenedor (`/up` y un login).
3. En el proxy, rutear `aosc.gob.ar/api/v1/*` al contenedor. La app no
   necesita una versión nueva porque su base URL ya es
   `https://aosc.gob.ar/api/v1`. Para volver atrás, se quita la regla.
4. Push: sacar `notificaciones:enviar` de `app/Console/Kernel.php` de
   aosc_front y recién entonces poner `NOTIFICACIONES_HABILITADAS=true`.
5. Más adelante se pueden quitar de aosc_front las rutas de `routes/api.php` y
   los controladores `Api\*`.
