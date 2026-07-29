package access

import (
	"net/http"
	"strings"
	"sync"
)

type Checker struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}

func New(keys []string) *Checker {
	return &Checker{keys: keySet(keys)}
}

// Replace swaps the accepted gateway API keys.
func (c *Checker) Replace(keys []string) {
	m := keySet(keys)
	c.mu.Lock()
	c.keys = m
	c.mu.Unlock()
}

func keySet(keys []string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k != "" {
			m[k] = struct{}{}
		}
	}
	return m
}

func (c *Checker) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public: health, root, and embedded monitor assets. /api/logs* still
		// require a gateway key (the browser prompts once and stores it locally).
		if r.URL.Path == "/healthz" || r.URL.Path == "/" || r.URL.Path == "/dashboard" || r.URL.Path == "/dashboard.html" || strings.HasPrefix(r.URL.Path, "/dashboard/") {
			next.ServeHTTP(w, r)
			return
		}
		key := extractKey(r)
		if key == "" {
			writeUnauthorized(w, "missing api key")
			return
		}
		c.mu.RLock()
		_, ok := c.keys[key]
		c.mu.RUnlock()
		if !ok {
			writeUnauthorized(w, "invalid api key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func extractKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(auth, prefix) {
			return strings.TrimSpace(auth[len(prefix):])
		}
		return strings.TrimSpace(auth)
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	if k := r.Header.Get("api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"` + msg + `","type":"authentication_error","code":"invalid_api_key"}}`))
}
