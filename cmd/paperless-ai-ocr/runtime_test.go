package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProductionHTTPServerBounds(t *testing.T) {
	server := productionHTTPServer(http.NewServeMux())
	if server.ReadHeaderTimeout <= 0 || server.ReadTimeout <= 0 || server.WriteTimeout <= 2*time.Second || server.IdleTimeout <= 0 {
		t.Fatalf("server timeouts = header %s read %s write %s idle %s",
			server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout, server.IdleTimeout)
	}
}

func TestStopSignalNotificationAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	done := stopSignalNotificationAfterCancellation(ctx, func() { calls.Add(1) })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signal notification was not stopped after cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("stop calls = %d, want 1", calls.Load())
	}
}

func TestRunStderrDoesNotExposeConfigurationCanaries(t *testing.T) {
	canaries := map[string]string{
		"PAPERLESS_URL":            "https://CANARY-paperless.example/private",
		"PAPERLESS_API_TOKEN":      "CANARY-paperless-token",
		"AI_BASE_URL":              "https://CANARY-provider.example/v1",
		"AI_API_KEY":               "CANARY-ai-key",
		"AI_MODEL":                 "CANARY-model",
		"WEBHOOK_TOKEN":            "CANARY-webhook-token",
		"PAPERLESS_AI_WEBHOOK_URL": "https://CANARY-paperless-ai.example/webhook",
		"PAPERLESS_AI_WEBHOOK_KEY": "CANARY-paperless-ai-key",
		"HTTP_PORT":                "CANARY-invalid-port",
	}
	for name, value := range canaries {
		t.Setenv(name, value)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "{\"level\":\"error\",\"event\":\"background_failure\",\"category\":\"configuration\"}\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	for _, canary := range canaries {
		if strings.Contains(stderr.String(), canary) {
			t.Errorf("stderr disclosed %q", canary)
		}
	}
	if strings.Contains(stderr.String(), "918273645") {
		t.Error("stderr disclosed document ID")
	}
}
