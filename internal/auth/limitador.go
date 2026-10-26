package auth

import (
	"sync"
	"time"
)

// Limitador cuenta intentos fallidos de login por clave (email|ip) en una
// ventana fija, como RateLimiter::hit() de Laravel (5 intentos, 60 s).
type Limitador struct {
	mu       sync.Mutex
	max      int
	ventana  time.Duration
	intentos map[string]*registroIntentos
}

type registroIntentos struct {
	cantidad int
	vence    time.Time
}

func NuevoLimitador(max int, ventana time.Duration) *Limitador {
	return &Limitador{max: max, ventana: ventana, intentos: map[string]*registroIntentos{}}
}

// Bloqueado devuelve los segundos que faltan si la clave superó el máximo.
func (l *Limitador) Bloqueado(clave string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.intentos[clave]
	if !ok || time.Now().After(r.vence) {
		return 0, false
	}
	if r.cantidad < l.max {
		return 0, false
	}
	seg := int(time.Until(r.vence).Seconds()) + 1
	return seg, true
}

func (l *Limitador) Fallo(clave string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ahora := time.Now()
	r, ok := l.intentos[clave]
	if !ok || ahora.After(r.vence) {
		l.intentos[clave] = &registroIntentos{cantidad: 1, vence: ahora.Add(l.ventana)}
		return
	}
	r.cantidad++
}

func (l *Limitador) Limpiar(clave string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.intentos, clave)
}

// Purgar elimina las claves vencidas para que el mapa no crezca sin límite.
func (l *Limitador) Purgar() {
	l.mu.Lock()
	defer l.mu.Unlock()
	ahora := time.Now()
	for k, r := range l.intentos {
		if ahora.After(r.vence) {
			delete(l.intentos, k)
		}
	}
}
