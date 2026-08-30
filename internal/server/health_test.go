package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthAndReadiness(t *testing.T) {
	readiness := NewReadiness()
	handler := NewHealthHandler(readiness)

	for _, test := range []struct {
		name       string
		path       string
		method     string
		wantStatus int
		wantBody   string
	}{
		{name: "health", path: "/health", method: http.MethodGet, wantStatus: http.StatusOK, wantBody: "ok\n"},
		{name: "not ready", path: "/ready", method: http.MethodGet, wantStatus: http.StatusServiceUnavailable, wantBody: "not ready\n"},
		{name: "method restricted", path: "/health", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed, wantBody: "Method Not Allowed\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("response = (%d, %q), want (%d, %q)", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			if got, want := response.Header().Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
		})
	}

	readiness.Set(true)
	request := httptest.NewRequest(http.MethodGet, "/ready", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "ready\n" {
		t.Fatalf("ready response = (%d, %q)", response.Code, response.Body.String())
	}
	readiness.Set(false)
	request = httptest.NewRequest(http.MethodGet, "/ready", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "not ready\n" {
		t.Fatalf("shutdown response = (%d, %q)", response.Code, response.Body.String())
	}
}
