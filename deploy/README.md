# aosc-api en producción (Vi-App-A)

```
~/apps/go/aosc27api/                  clone de keloxers/aosc27api
  deploy/.env.aosc                    config AOSC   (600, fuera de git)
  deploy/.env.covesa                  config COVESA (600, fuera de git)
  deploy/secrets/aosc/firebase-credentials.json   cuenta de servicio FCM (600, fuera de git)
```

```bash
cd ~/apps/go/aosc27api/deploy && BIND_IP=10.53.112.6 docker compose up -d --build
```

Puertos 6880 (aosc) y 6881 (covesa), solo en la IP privada. Con ufw, `route allow` solo desde Vi-Proxy (10.53.112.3).
Bases en Vi-MariaDb: `aosc` y `covesa`, las mismas que usan la gestión (aosc27) y el sitio público (aosc27Front).
Los `DB_*`, `MAIL_*` y `ARCHIVOS_URL` salen del `.env` de `web-<instancia>`. Cada instancia lleva su propio
`JWT_SECRET` (`openssl rand -hex 32`).

`INSTANCIA` y `HTTP_ADDR` los fija el compose. COVESA corre sin push: su base no tiene `dispositivos` ni
`notificaciones_*`, y `/api/v1/dispositivos` responde OK sin guardar nada.

Corte (por instancia): probar `curl http://10.53.112.6:6880/up`, después rutear en Vi-Proxy
`<dominio>/api/v1/*` → `10.53.112.6:688x`. Para volver atrás, se quita la regla.
