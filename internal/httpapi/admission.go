package httpapi

import (
	"net"
	"net/http"
	"sync"
	"time"

	"avito-kitchen/internal/domain"
)

// Лимиты действуют внутри одного процесса API; память и число одновременных запросов также ограничены.
const (
	peerRate    = 120.0
	peerBurst   = 240.0
	globalRate  = 200.0
	globalBurst = 400.0
	maxPeers    = 10000
	maxInFlight = 64
)

type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) refill(now time.Time, rate, burst float64) {
	b.tokens = min(burst, b.tokens+max(0, now.Sub(b.last).Seconds())*rate)
	b.last = now
}

type admission struct {
	mu          sync.Mutex
	peers       map[string]*bucket
	global      bucket
	cleaned     time.Time
	slots       chan struct{}
	healthSlots chan struct{}
}

func newAdmission() *admission {
	now := time.Now()
	return &admission{peers: make(map[string]*bucket), global: bucket{globalBurst, now}, cleaned: now, slots: make(chan struct{}, maxInFlight), healthSlots: make(chan struct{}, 2)}
}

func (a *admission) allow(peer string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.cleaned) >= time.Minute {
		for key, b := range a.peers {
			if now.Sub(b.last) >= time.Minute {
				delete(a.peers, key)
			}
		}
		a.cleaned = now
	}
	a.global.refill(now, globalRate, globalBurst)
	if a.global.tokens < 1 {
		return false
	}
	b := a.peers[peer]
	if b == nil {
		if len(a.peers) >= maxPeers {
			return false
		}
		b = &bucket{peerBurst, now}
		a.peers[peer] = b
	}
	b.refill(now, peerRate, peerBurst)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	a.global.tokens--
	return true
}

func (a *admission) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == "GET" || r.Method == "HEAD") && r.URL.Path == "/healthz" {
			select {
			case a.healthSlots <- struct{}{}:
				defer func() { <-a.healthSlots }()
				next.ServeHTTP(w, r)
				return
			default:
			}
		} else {
			peer, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				peer = r.RemoteAddr
			}
			if a.allow(peer, time.Now()) {
				select {
				case a.slots <- struct{}{}:
					defer func() { <-a.slots }()
					next.ServeHTTP(w, r)
					return
				default:
				}
			}
		}
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		write(w, http.StatusTooManyRequests, map[string]any{"error": domain.Error{Code: "rate_limited", Message: "too many requests; retry after one second"}})
	})
}
