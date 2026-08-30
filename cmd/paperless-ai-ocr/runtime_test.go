package main

import (
	"context"
	"net/http"
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
