// Package observability provides bounded aggregate service metrics.
package observability

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/reconcile"
)

// JobOutcome is a fixed job completion result label.
type JobOutcome string

const (
	OutcomeSuccess JobOutcome = "success"
	OutcomeFailure JobOutcome = "failure"
)

// ProviderOperation is a fixed provider latency operation label.
type ProviderOperation string

const (
	OperationProbe      ProviderOperation = "probe"
	OperationTranscribe ProviderOperation = "transcribe"
)

var queueStates = [...]queue.State{
	queue.StatePending,
	queue.StateProcessing,
	queue.StateRetry,
	queue.StateCompleted,
	queue.StateFailed,
}

// Metrics stores aggregate counters and gauges without unbounded labels.
type Metrics struct {
	mu                    sync.RWMutex
	queueDepth            map[queue.State]int64
	jobOutcomes           map[JobOutcome]uint64
	retries               uint64
	providerLatency       map[ProviderOperation]time.Duration
	providerRequests      map[ProviderOperation]uint64
	processedPages        uint64
	renderedBytes         uint64
	recoveredLeases       uint64
	reconciliationResults map[string]uint64
}

// NewMetrics creates an empty aggregate metrics collector.
func NewMetrics() *Metrics {
	return &Metrics{
		queueDepth:            make(map[queue.State]int64),
		jobOutcomes:           make(map[JobOutcome]uint64),
		providerLatency:       make(map[ProviderOperation]time.Duration),
		providerRequests:      make(map[ProviderOperation]uint64),
		reconciliationResults: make(map[string]uint64),
	}
}

func (metrics *Metrics) SetQueueDepth(depth map[queue.State]int64) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	for _, state := range queueStates {
		metrics.queueDepth[state] = max(depth[state], 0)
	}
}

func (metrics *Metrics) RecordJobOutcome(outcome JobOutcome) {
	if outcome != OutcomeSuccess && outcome != OutcomeFailure {
		return
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.jobOutcomes[outcome]++
}

func (metrics *Metrics) RecordRetry() {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.retries++
}

func (metrics *Metrics) RecordProviderLatency(operation ProviderOperation, duration time.Duration) {
	if operation != OperationProbe && operation != OperationTranscribe || duration < 0 {
		return
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.providerLatency[operation] += duration
	metrics.providerRequests[operation]++
}

func (metrics *Metrics) RecordProcessedPages(pages int) {
	if pages <= 0 {
		return
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.processedPages += uint64(pages)
}

func (metrics *Metrics) RecordRenderedBytes(bytes int64) {
	if bytes <= 0 {
		return
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.renderedBytes += uint64(bytes)
}

func (metrics *Metrics) RecordRecoveredLeases(count int64) {
	if count <= 0 {
		return
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.recoveredLeases += uint64(count)
}

func (metrics *Metrics) RecordReconciliation(report reconcile.Report) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.reconciliationResults["candidates_resolved"] += uint64(max(report.CandidatesResolved, 0))
	metrics.reconciliationResults["candidates_discarded"] += uint64(max(report.CandidatesDiscarded, 0))
	metrics.reconciliationResults["documents_seen"] += uint64(max(report.DocumentsSeen, 0))
	metrics.reconciliationResults["jobs_created"] += uint64(max(report.JobsCreated, 0))
	metrics.reconciliationResults["pages_processed"] += uint64(max(report.PagesProcessed, 0))
	if report.ScanComplete {
		metrics.reconciliationResults["scans_completed"]++
	}
}

// ServeHTTP writes deterministic Prometheus text exposition.
func (metrics *Metrics) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(response, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	var body strings.Builder
	for _, state := range queueStates {
		fmt.Fprintf(&body, "paperless_ai_ocr_queue_depth{state=%q} %d\n", state, metrics.queueDepth[state])
	}
	for _, outcome := range []JobOutcome{OutcomeSuccess, OutcomeFailure} {
		fmt.Fprintf(&body, "paperless_ai_ocr_job_outcomes_total{outcome=%q} %d\n", outcome, metrics.jobOutcomes[outcome])
	}
	fmt.Fprintf(&body, "paperless_ai_ocr_retries_total %d\n", metrics.retries)
	for _, operation := range []ProviderOperation{OperationProbe, OperationTranscribe} {
		fmt.Fprintf(&body, "paperless_ai_ocr_provider_latency_seconds{operation=%q} %.9f\n", operation, metrics.providerLatency[operation].Seconds())
		fmt.Fprintf(&body, "paperless_ai_ocr_provider_requests_total{operation=%q} %d\n", operation, metrics.providerRequests[operation])
	}
	fmt.Fprintf(&body, "paperless_ai_ocr_processed_pages_total %d\n", metrics.processedPages)
	fmt.Fprintf(&body, "paperless_ai_ocr_rendered_bytes_total %d\n", metrics.renderedBytes)
	fmt.Fprintf(&body, "paperless_ai_ocr_recovered_leases_total %d\n", metrics.recoveredLeases)
	for _, result := range []string{"candidates_resolved", "candidates_discarded", "documents_seen", "jobs_created", "pages_processed", "scans_completed"} {
		fmt.Fprintf(&body, "paperless_ai_ocr_reconciliation_results_total{result=%q} %d\n", result, metrics.reconciliationResults[result])
	}
	response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = response.Write([]byte(body.String()))
}
