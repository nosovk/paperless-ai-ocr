package main

import (
	"bytes"
	"context"
	"errors"
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

func TestNormalizeSignalCancellationStopsNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	_, done := normalizeSignalCancellation(ctx, func() { calls.Add(1) })
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

func TestNormalizeSignalCancellation(t *testing.T) {
	signalCtx, cancelSignal := context.WithCancelCause(context.Background())
	var stopCalls atomic.Int64
	ctx, done := normalizeSignalCancellation(signalCtx, func() { stopCalls.Add(1) })

	cancelSignal(errors.New("platform signal cancellation cause"))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signal cancellation was not normalized")
	}
	if cause := context.Cause(ctx); cause != context.Canceled {
		t.Fatalf("normalized cause = %v, want context.Canceled", cause)
	}
	if stopCalls.Load() != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls.Load())
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

func TestVersionOutputFailureIsSafe(t *testing.T) {
	const canary = "CANARY private stdout writer path"
	var stderr bytes.Buffer
	if code := run([]string{"--version"}, errorOutputWriter{err: errors.New(canary)}, &stderr); code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	if got, want := stderr.String(), "paperless-ai-ocr: output failed\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	if strings.Contains(stderr.String(), canary) {
		t.Fatal("stderr exposed output error")
	}
}

type errorOutputWriter struct{ err error }

func (writer errorOutputWriter) Write([]byte) (int, error) { return 0, writer.err }
