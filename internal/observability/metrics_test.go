package observability

import (
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
