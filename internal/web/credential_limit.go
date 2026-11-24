package web

import (
	"crypto/sha256"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxCredentialWindows = 256 // Each of the peer and account pools has its own budget.

type credentialWindow struct {
	start time.Time
	count int
}

// credentialLimiter bounds both attempts and bookkeeping on a small shared host.
// Forwarded client identities require an explicitly trusted socket peer.
type credentialLimiter struct {
	mu      sync.Mutex
	windows map[[32]byte]credentialWindow
}

func (l *credentialLimiter) allow(key string, limit int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = make(map[[32]byte]credentialWindow)
	}
	hash := sha256.Sum256([]byte(key))
	window, exists := l.windows[hash]
	if !exists && len(l.windows) >= maxCredentialWindows {
		// Reclaim expired counters, never live quotas that an attacker could evict.
		for candidate, value := range l.windows {
			if now.Sub(value.start) >= time.Minute {
				delete(l.windows, candidate)
			}
		}
		if len(l.windows) >= maxCredentialWindows {
			return false
		}
	}
	if window.start.IsZero() || now.Sub(window.start) >= time.Minute {
		window = credentialWindow{start: now}
	}
	if window.count >= limit {
		return false
	}
	window.count++
	l.windows[hash] = window
	return true
}

func credentialPeer(r *http.Request, trustedProxies []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return host
	}
	for _, trusted := range trustedProxies {
		if peer.Equal(net.ParseIP(trusted)) {
			if client := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); client != nil {
				return client.String()
			}
			break
		}
	}
	return peer.String()
}

func rejectCredentialRate(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many authentication attempts; retry in one minute"})
}
