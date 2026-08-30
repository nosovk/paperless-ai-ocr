package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/observability"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/reconcile"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	"github.com/nosovk/paperless-ai-ocr/internal/securelog"
	"github.com/nosovk/paperless-ai-ocr/internal/server"
	"github.com/nosovk/paperless-ai-ocr/internal/worker"
)

func TestRunStartupOrderAndReadiness(t *testing.T) {
	readiness := server.NewReadiness()
	initialize := make(chan struct{})
	reconcile := make(chan struct{})
	runtime := &fakeRuntime{events: make(chan string, 16), claimResults: []claimResult{{}}, initialize: initialize, reconcile: reconcile}
	listener := newTestListener(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Runtime: runtime, Readiness: readiness, Metrics: observability.NewMetrics(),
			Listener: listener, HTTPServer: &http.Server{}, PollInterval: time.Hour,
			IdleInterval: time.Hour, ShutdownTimeout: time.Second,
		})
	}()

	for _, want := range []string{"recover", "ping", "probe"} {
		select {
		case got := <-runtime.events:
			if got != want {
				t.Fatalf("startup event = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	if readiness.Ready() {
		t.Fatal("readiness became true before runtime initialization")
	}
	close(initialize)
	for _, want := range []string{"initialize", "reconcile"} {
		select {
		case got := <-runtime.events:
			if got != want {
				t.Fatalf("startup event = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	if readiness.Ready() {
		t.Fatal("readiness became true before initial reconciliation completed")
	}
	select {
	case event := <-runtime.events:
		t.Fatalf("unexpected startup event before reconciliation completed: %q", event)
	default:
	}
	close(reconcile)
	select {
	case got := <-runtime.events:
		if got != "claim" {
			t.Fatalf("startup event = %q, want claim", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for claim")
	}
	if !readiness.Ready() {
		t.Fatal("readiness = false after initialization")
	}

	cancel(context.Canceled)
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if readiness.Ready() {
		t.Fatal("readiness remained true after shutdown")
	}
	runtime.assertOrder(t, "cancel", "close")
}

func TestRunLogsOnlySafeLifecycleAndJobFields(t *testing.T) {
	const (
		documentID = int64(424242)
		checksum   = "CANARY-source-checksum"
		owner      = "CANARY-lease-owner"
		model      = "CANARY-model-name"
		content    = "CANARY-transcription-output"
	)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancelCause(context.Background())
	runtime := &fakeRuntime{
		events: make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{
			ID: 1, DocumentID: documentID, SourceChecksum: checksum, Attempts: 1,
			LeaseOwner: owner, Model: model, State: queue.StateProcessing,
		}, ok: true}},
	}
	runtime.process = func(context.Context, queue.Job) (worker.Result, error) {
		defer cancel(context.Canceled)
		return worker.Result{DocumentID: documentID, Content: content, SourceChecksum: checksum}, nil
	}
	options := testOptions(t, runtime)
	options.Logger = securelog.New(&logs)
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	output := logs.String()
	for _, want := range []string{
		`"event":"startup"`, `"event":"ready"`, `"event":"job_claimed"`,
		`"document_id":424242`, `"event":"job_finished"`, `"state":"completed"`,
		`"event":"shutdown"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("logs missing %q: %s", want, output)
		}
	}
	for _, canary := range []string{checksum, owner, model, content} {
		if strings.Contains(output, canary) {
			t.Errorf("logs disclosed %q: %s", canary, output)
		}
	}
}

func TestRunLogsOnlySafeBackgroundFailureCategory(t *testing.T) {
	const canary = "CANARY private reconciliation body and token"
	var logs bytes.Buffer
	runtime := &fakeRuntime{events: make(chan string, 16), reconcileErr: errors.New(canary)}
	options := testOptions(t, runtime)
	options.Logger = securelog.New(&logs)
	if err := Run(context.Background(), options); err != errBackground {
		t.Fatalf("Run() error = %v, want background sentinel", err)
	}
	if output := logs.String(); !strings.Contains(output, `"event":"background_failure","category":"internal"`) || strings.Contains(output, canary) {
		t.Fatalf("logs = %s", output)
	}
}

func TestRunInitialReconciliationFailureNeverBecomesReady(t *testing.T) {
	readiness := server.NewReadiness()
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{}},
		reconcileErr: errors.New("CANARY private reconciliation failure"),
	}
	err := Run(context.Background(), Options{
		Runtime: runtime, Readiness: readiness, Metrics: observability.NewMetrics(),
		Listener: newTestListener(t), HTTPServer: &http.Server{}, PollInterval: time.Hour,
		IdleInterval: time.Hour, ShutdownTimeout: time.Second,
	})
	if err == nil || err.Error() != "internal: background operation failed" {
		t.Fatalf("Run() error = %v", err)
	}
	if readiness.Ready() {
		t.Fatal("readiness became true after failed initial reconciliation")
	}
	if slices.Contains(runtime.ordered, "claim") {
		t.Fatalf("worker claimed after failed reconciliation: %v", runtime.ordered)
	}
}

func TestRunReleasesActiveLeaseOnShutdown(t *testing.T) {
	started := make(chan struct{})
	readiness := server.NewReadiness()
	runtime := &fakeRuntime{
		events:            make(chan string, 16),
		claimResults:      []claimResult{{job: queue.Job{ID: 7, Attempts: 2, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
		state:             queue.StateProcessing,
		stateFound:        true,
		releaseOK:         true,
		stateAfterRelease: queue.StateRetry,
		process: func(ctx context.Context, _ queue.Job) (worker.Result, error) {
			close(started)
			<-ctx.Done()
			if readiness.Ready() {
				return worker.Result{}, errors.New("readiness remained true during cancellation")
			}
			return worker.Result{}, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		options := testOptions(t, runtime)
		options.Readiness = readiness
		done <- Run(ctx, options)
	}()
	<-started
	cancel(context.Canceled)
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if runtime.released.ID != 7 || runtime.released.Attempts != 2 || runtime.released.LeaseOwner != "worker" {
		t.Errorf("released = %+v", runtime.released)
	}
	runtime.assertOrder(t, "cancel", "release", "close")
}

func TestRunFailsWhenActiveLeaseReleaseFails(t *testing.T) {
	started := make(chan struct{})
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{ID: 7, Attempts: 2, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
		process: func(ctx context.Context, _ queue.Job) (worker.Result, error) {
			close(started)
			<-ctx.Done()
			return worker.Result{}, ctx.Err()
		},
		state:      queue.StateProcessing,
		stateFound: true,
		releaseErr: errors.New("CANARY private database failure"),
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, testOptions(t, runtime)) }()
	<-started
	cancel(context.Canceled)
	if err := <-done; err == nil || err.Error() != "internal: shutdown failed" {
		t.Fatalf("Run() error = %v, want safe shutdown failure", err)
	}
	runtime.assertOrder(t, "cancel", "release", "close")
}

func TestRunBoundsShutdownWithoutClosingActiveRuntime(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{ID: 7, Attempts: 2, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
		state:        queue.StateProcessing,
		stateFound:   true,
		process: func(context.Context, queue.Job) (worker.Result, error) {
			close(started)
			<-release
			close(exited)
			return worker.Result{}, context.Canceled
		},
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		options := testOptions(t, runtime)
		options.ShutdownTimeout = 25 * time.Millisecond
		done <- Run(ctx, options)
	}()
	<-started
	startedAt := time.Now()
	cancel(context.Canceled)
	select {
	case err := <-done:
		if err == nil || err.Error() != "internal: shutdown failed" {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not honor shutdown timeout")
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Errorf("shutdown elapsed = %s, want bounded", elapsed)
	}
	if runtime.closed {
		t.Fatal("runtime closed while worker loop was still active")
	}
	close(release)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("stuck worker did not exit after test cleanup")
	}
}

func TestRunAcceptsStaleReleaseAfterConfirmedRetry(t *testing.T) {
	started := make(chan struct{})
	metrics := observability.NewMetrics()
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{ID: 7, Attempts: 2, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
		process: func(ctx context.Context, _ queue.Job) (worker.Result, error) {
			close(started)
			<-ctx.Done()
			return worker.Result{}, ctx.Err()
		},
		state:      queue.StateRetry,
		stateFound: true,
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		options := testOptions(t, runtime)
		options.Metrics = metrics
		done <- Run(ctx, options)
	}()
	<-started
	cancel(context.Canceled)
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if runtime.released.ID != 0 {
		t.Errorf("Release() called for durably retried job: %+v", runtime.released)
	}
	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), "paperless_ai_ocr_retries_total 1\n") {
		t.Fatalf("metrics = %q", response.Body.String())
	}
}

func TestRunContinuesAfterWorkerRetryTransition(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	var calls int
	runtime := &fakeRuntime{
		events: make(chan string, 16),
		claimResults: []claimResult{
			{job: queue.Job{ID: 1, Attempts: 1, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true},
			{job: queue.Job{ID: 2, Attempts: 1, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true},
		},
	}
	runtime.state = queue.StateRetry
	runtime.stateFound = true
	runtime.process = func(context.Context, queue.Job) (worker.Result, error) {
		calls++
		if calls == 1 {
			return worker.Result{}, errors.New("retry already scheduled")
		}
		cancel(context.Canceled)
		return worker.Result{}, context.Canceled
	}
	if err := Run(ctx, testOptions(t, runtime)); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("Process() calls = %d, want 2", calls)
	}
}

func TestRunRecordsConfirmedDeadlineRetry(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	metrics := observability.NewMetrics()
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{ID: 1, Attempts: 1, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
		state:        queue.StateRetry,
		stateFound:   true,
	}
	runtime.process = func(context.Context, queue.Job) (worker.Result, error) {
		defer cancel(context.Canceled)
		return worker.Result{}, context.DeadlineExceeded
	}
	options := testOptions(t, runtime)
	options.Metrics = metrics
	if err := Run(ctx, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), "paperless_ai_ocr_retries_total 1\n") {
		t.Fatalf("metrics = %q", response.Body.String())
	}
}

func TestRunMetricsScrapeCurrentQueueDepth(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	metrics := observability.NewMetrics()
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{}},
		depth:        map[queue.State]int64{queue.StatePending: 1},
	}
	done := make(chan error, 1)
	go func() {
		options := testOptions(t, runtime)
		options.Metrics = metrics
		options.IdleInterval = time.Hour
		done <- Run(ctx, options)
	}()
	for range 6 {
		<-runtime.events
	}
	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), `paperless_ai_ocr_queue_depth{state="pending"} 1`) {
		t.Fatalf("startup metrics = %q", response.Body.String())
	}
	runtime.mu.Lock()
	runtime.depth = map[queue.State]int64{queue.StatePending: 2}
	runtime.mu.Unlock()
	response = httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), `paperless_ai_ocr_queue_depth{state="pending"} 2`) {
		t.Fatalf("updated metrics = %q", response.Body.String())
	}
	cancel(context.Canceled)
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestWorkerMetricObserversUseAggregateTranscribeMetrics(t *testing.T) {
	metrics := observability.NewMetrics()
	observers := workerMetricObservers(metrics)
	observers.providerAttempt(250 * time.Millisecond)
	observers.processedPages(2)
	observers.renderedBytes(18)
	response := httptest.NewRecorder()
	metrics.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		`paperless_ai_ocr_provider_requests_total{operation="transcribe"} 1`,
		"paperless_ai_ocr_processed_pages_total 2",
		"paperless_ai_ocr_rendered_bytes_total 18",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q: %q", want, body)
		}
	}
}

func TestRunStopsWhenWorkerErrorLeavesLeaseActive(t *testing.T) {
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{ID: 1, Attempts: 1, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
		process: func(context.Context, queue.Job) (worker.Result, error) {
			return worker.Result{}, errors.New("CANARY private worker failure")
		},
		state: queue.StateProcessing, stateFound: true,
	}
	err := Run(context.Background(), testOptions(t, runtime))
	if err == nil || err.Error() != "internal: background operation failed" {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunFinalizationPaths(t *testing.T) {
	terminal := errors.New("terminal OCR")
	for _, test := range []struct {
		name         string
		result       worker.Result
		processErr   error
		terminal     bool
		wantFinalize string
		state        queue.State
	}{
		{name: "success", result: worker.Result{JobID: 1}, wantFinalize: "success"},
		{name: "terminal", processErr: terminal, terminal: true, wantFinalize: "failure"},
		{name: "retry", processErr: errors.New("retry transitioned"), state: queue.StateRetry},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			runtime := &fakeRuntime{
				events: make(chan string, 16), terminalErr: terminal,
				claimResults: []claimResult{{job: queue.Job{ID: 1, Attempts: 1, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}, {}},
				state:        test.state, stateFound: test.state != "",
			}
			runtime.process = func(context.Context, queue.Job) (worker.Result, error) {
				defer cancel(context.Canceled)
				return test.result, test.processErr
			}
			if err := Run(ctx, testOptions(t, runtime)); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if runtime.finalized != test.wantFinalize {
				t.Errorf("finalized = %q, want %q", runtime.finalized, test.wantFinalize)
			}
		})
	}
}

func TestRunReturnsSafeFatalCause(t *testing.T) {
	runtime := &fakeRuntime{events: make(chan string, 16), reconcileErr: errors.New("CANARY private failure")}
	err := Run(context.Background(), testOptions(t, runtime))
	if err != errBackground {
		t.Fatalf("Run() error = %v, want trusted internal cause", err)
	}
}

func TestRunSanitizesParentCancellationCauses(t *testing.T) {
	const canary = "CANARY secret cancellation cause"
	for _, test := range []struct {
		name      string
		cause     error
		wantError bool
	}{
		{name: "categorized", cause: saferr.New(saferr.CategoryInternal, canary), wantError: true},
		{name: "ordinary", cause: errors.New(canary), wantError: true},
		{name: "wrapped canceled", cause: fmt.Errorf("%s: %w", canary, context.Canceled), wantError: true},
		{name: "wrapped deadline", cause: fmt.Errorf("%s: %w", canary, context.DeadlineExceeded), wantError: true},
		{name: "canceled", cause: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			runtime := &fakeRuntime{events: make(chan string, 16), claimResults: []claimResult{{}}}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, testOptions(t, runtime)) }()
			for range 6 {
				<-runtime.events
			}
			cancel(test.cause)
			err := <-done
			if !test.wantError {
				if err != nil {
					t.Fatalf("Run() error = %v, want nil", err)
				}
				return
			}
			if err != errParentCanceled {
				t.Fatalf("Run() error = %v, want fixed cancellation sentinel", err)
			}
			for _, formatted := range []string{
				err.Error(), fmt.Sprintf("%s", err), fmt.Sprintf("%v", err),
				fmt.Sprintf("%+v", err), fmt.Sprintf("%q", err),
			} {
				if strings.Contains(formatted, canary) {
					t.Errorf("formatted error exposed canary: %q", formatted)
				}
			}
			for current := err; current != nil; current = errors.Unwrap(current) {
				if strings.Contains(current.Error(), canary) {
					t.Errorf("unwrap chain exposed canary: %q", current.Error())
				}
			}
			var safeError *saferr.Error
			if !errors.As(err, &safeError) || safeError.Error() != "internal: application canceled" {
				t.Fatalf("errors.As() = %v, safe error = %v", errors.As(err, &safeError), safeError)
			}
			if externalSafeError, ok := test.cause.(*saferr.Error); ok && safeError == externalSafeError {
				t.Fatal("errors.As() reached external cancellation cause")
			}
			if errors.Is(err, test.cause) {
				t.Fatal("external cancellation cause remained traversable")
			}
		})
	}
}

type claimResult struct {
	job queue.Job
	ok  bool
	err error
}

type fakeRuntime struct {
	mu                sync.Mutex
	events            chan string
	ordered           []string
	claimResults      []claimResult
	process           func(context.Context, queue.Job) (worker.Result, error)
	terminalErr       error
	reconcileErr      error
	initialize        <-chan struct{}
	reconcile         <-chan struct{}
	state             queue.State
	stateFound        bool
	stateAfterRelease queue.State
	releaseOK         bool
	releaseErr        error
	released          queue.Job
	finalized         string
	depth             map[queue.State]int64
	closed            bool
}

func (runtime *fakeRuntime) event(value string) {
	runtime.mu.Lock()
	runtime.ordered = append(runtime.ordered, value)
	runtime.mu.Unlock()
	if runtime.events != nil {
		runtime.events <- value
	}
}

func (runtime *fakeRuntime) Recover(context.Context) (int64, error) {
	runtime.event("recover")
	return 1, nil
}
func (runtime *fakeRuntime) Ping(context.Context) error  { runtime.event("ping"); return nil }
func (runtime *fakeRuntime) Probe(context.Context) error { runtime.event("probe"); return nil }
func (runtime *fakeRuntime) Initialize(ctx context.Context) error {
	if runtime.initialize != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-runtime.initialize:
		}
	}
	runtime.event("initialize")
	return nil
}
func (runtime *fakeRuntime) Reconcile(context.Context) (reconcile.Report, error) {
	runtime.event("reconcile")
	if runtime.reconcile != nil {
		<-runtime.reconcile
	}
	return reconcile.Report{}, runtime.reconcileErr
}
func (runtime *fakeRuntime) Claim(context.Context) (queue.Job, bool, error) {
	runtime.event("claim")
	if len(runtime.claimResults) == 0 {
		return queue.Job{}, false, nil
	}
	result := runtime.claimResults[0]
	runtime.claimResults = runtime.claimResults[1:]
	return result.job, result.ok, result.err
}
func (runtime *fakeRuntime) Process(ctx context.Context, job queue.Job) (worker.Result, error) {
	runtime.event("process")
	if runtime.process != nil {
		return runtime.process(ctx, job)
	}
	return worker.Result{}, nil
}
func (runtime *fakeRuntime) Terminal(_ context.Context, _ queue.Job, err error) bool {
	return runtime.terminalErr != nil && errors.Is(err, runtime.terminalErr)
}
func (runtime *fakeRuntime) FinalizeSuccess(context.Context, queue.Job, worker.Result) error {
	runtime.finalized = "success"
	return nil
}
func (runtime *fakeRuntime) FinalizeFailure(context.Context, queue.Job) error {
	runtime.finalized = "failure"
	return nil
}
func (runtime *fakeRuntime) Release(_ context.Context, job queue.Job) (bool, error) {
	runtime.event("release")
	runtime.released = job
	if runtime.stateAfterRelease != "" {
		runtime.state = runtime.stateAfterRelease
	}
	return runtime.releaseOK, runtime.releaseErr
}
func (runtime *fakeRuntime) QueueDepth(context.Context) (map[queue.State]int64, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.depth, nil
}
func (runtime *fakeRuntime) State(context.Context, queue.Job) (queue.State, bool, error) {
	return runtime.state, runtime.stateFound, nil
}
func (runtime *fakeRuntime) Cancel() { runtime.event("cancel") }
func (runtime *fakeRuntime) Close() error {
	runtime.event("close")
	runtime.closed = true
	return nil
}

func (runtime *fakeRuntime) assertOrder(t *testing.T, values ...string) {
	t.Helper()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	positions := make([]int, len(values))
	for index, value := range values {
		positions[index] = slices.Index(runtime.ordered, value)
		if positions[index] < 0 {
			t.Fatalf("events %v missing %q", runtime.ordered, value)
		}
		if index > 0 && positions[index] <= positions[index-1] {
			t.Fatalf("events %v do not order %v", runtime.ordered, values)
		}
	}
}

func testOptions(t *testing.T, runtime Runtime) Options {
	t.Helper()
	return Options{
		Runtime: runtime, Readiness: server.NewReadiness(), Metrics: observability.NewMetrics(),
		Listener: newTestListener(t), HTTPServer: &http.Server{}, PollInterval: time.Hour,
		IdleInterval: time.Millisecond, ShutdownTimeout: time.Second,
	}
}

func newTestListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

var _ = httptest.NewRecorder
