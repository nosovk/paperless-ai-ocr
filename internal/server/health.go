package server

import (
	"net/http"
	"sync/atomic"
)

// Readiness stores the dependency-aware runtime readiness state.
type Readiness struct {
	ready atomic.Bool
}

// NewReadiness constructs a readiness state that starts false.
func NewReadiness() *Readiness {
	return &Readiness{}
}

// Set changes the readiness state.
func (readiness *Readiness) Set(ready bool) {
	readiness.ready.Store(ready)
}

// Ready reports the current readiness state.
func (readiness *Readiness) Ready() bool {
	return readiness != nil && readiness.ready.Load()
}

// NewHealthHandler exposes local liveness and dependency-aware readiness.
func NewHealthHandler(readiness *Readiness) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(response http.ResponseWriter, _ *http.Request) {
		writePlain(response, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /ready", func(response http.ResponseWriter, _ *http.Request) {
		if readiness.Ready() {
			writePlain(response, http.StatusOK, "ready\n")
			return
		}
		writePlain(response, http.StatusServiceUnavailable, "not ready\n")
	})
	return mux
}

func writePlain(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(status)
	_, _ = response.Write([]byte(body))
}
