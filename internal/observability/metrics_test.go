package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/reconcile"
)

func TestMetricsExposeStableAggregateNamesOnly(t *testing.T) {
	metrics := NewMetrics()
	metrics.SetQueueDepth(map[queue.State]int64{queue.StatePending: 2, queue.StateProcessing: 1})
	metrics.RecordJobOutcome(OutcomeSuccess)
	metrics.RecordRetry()
	metrics.RecordProviderLatency(OperationProbe, 250*time.Millisecond)
	metrics.RecordProcessedPages(3)
	metrics.RecordRenderedBytes(4096)
	metrics.RecordRecoveredLeases(1)
	metrics.RecordReconciliation(reconcile.Report{CandidatesResolved: 1, DocumentsSeen: 2, JobsCreated: 1, PagesProcessed: 1, ScanComplete: true})

	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, name := range []string{
		"paperless_ai_ocr_queue_depth",
		"paperless_ai_ocr_job_outcomes_total",
		"paperless_ai_ocr_retries_total",
		"paperless_ai_ocr_provider_latency_seconds",
		"paperless_ai_ocr_processed_pages_total",
		"paperless_ai_ocr_rendered_bytes_total",
		"paperless_ai_ocr_reconciliation_results_total",
		"paperless_ai_ocr_recovered_leases_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics missing %q", name)
		}
	}
	for _, canary := range []string{"document-981", "secret.pdf", "https://private.invalid", "provider exploded"} {
		if strings.Contains(body, canary) {
			t.Errorf("metrics exposed forbidden value %q", canary)
		}
	}
	if got, want := response.Header().Get("Content-Type"), "text/plain; version=0.0.4; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

func TestMetricsRestrictMethods(t *testing.T) {
	response := httptest.NewRecorder()
	NewMetrics().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}

func TestMetricsCollectCurrentQueueDepthOnEveryScrape(t *testing.T) {
	depth := map[queue.State]int64{queue.StatePending: 1}
	metrics := NewMetrics()
	metrics.SetQueueDepthCollector(func(context.Context) (map[queue.State]int64, error) {
		return depth, nil
	})

	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), `paperless_ai_ocr_queue_depth{state="pending"} 1`) {
		t.Fatalf("first scrape = %q", response.Body.String())
	}
	depth = map[queue.State]int64{queue.StatePending: 2, queue.StateProcessing: 1}
	response = httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), `paperless_ai_ocr_queue_depth{state="pending"} 2`) ||
		!strings.Contains(response.Body.String(), `paperless_ai_ocr_queue_depth{state="processing"} 1`) {
		t.Fatalf("second scrape = %q", response.Body.String())
	}
}

func TestMetricsQueueCollectionFailureIsSafe(t *testing.T) {
	metrics := NewMetrics()
	metrics.SetQueueDepthCollector(func(context.Context) (map[queue.State]int64, error) {
		return nil, errors.New("CANARY private database URL")
	})
	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "metrics unavailable\n" {
		t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "CANARY") {
		t.Fatal("metrics response exposed collector error")
	}
}

func TestMetricsQueueCollectionIsBounded(t *testing.T) {
	metrics := NewMetrics()
	metrics.SetQueueDepthCollector(func(ctx context.Context) (map[queue.State]int64, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("collector context has no deadline")
		}
		return map[queue.State]int64{}, nil
	})
	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}
