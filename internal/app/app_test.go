package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/observability"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/reconcile"
	"github.com/nosovk/paperless-ai-ocr/internal/server"
	"github.com/nosovk/paperless-ai-ocr/internal/worker"
)

func TestRunStartupOrderAndReadiness(t *testing.T) {
	readiness := server.NewReadiness()
	initialize := make(chan struct{})
	runtime := &fakeRuntime{events: make(chan string, 16), claimResults: []claimResult{{}}, initialize: initialize}
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
	for _, want := range []string{"initialize", "reconcile", "claim"} {
		select {
		case got := <-runtime.events:
			if got != want {
				t.Fatalf("startup event = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
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

func TestRunReleasesActiveLeaseOnShutdown(t *testing.T) {
	started := make(chan struct{})
	readiness := server.NewReadiness()
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{ID: 7, Attempts: 2, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
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

func TestRunStopsWhenWorkerErrorLeavesLeaseActive(t *testing.T) {
	runtime := &fakeRuntime{
		events:       make(chan string, 16),
		claimResults: []claimResult{{job: queue.Job{ID: 1, Attempts: 1, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}},
		process: func(context.Context, queue.Job) (worker.Result, error) {
			return worker.Result{}, errors.New("CANARY private worker failure")
		},
		active: true,
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
	}{
		{name: "success", result: worker.Result{JobID: 1}, wantFinalize: "success"},
		{name: "terminal", processErr: terminal, terminal: true, wantFinalize: "failure"},
		{name: "retry", processErr: errors.New("retry transitioned")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			runtime := &fakeRuntime{
				events: make(chan string, 16), terminalErr: terminal,
				claimResults: []claimResult{{job: queue.Job{ID: 1, Attempts: 1, LeaseOwner: "worker", State: queue.StateProcessing}, ok: true}, {}},
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
	if err == nil || err.Error() != "internal: background operation failed" {
		t.Fatalf("Run() error = %v", err)
	}
}

type claimResult struct {
	job queue.Job
	ok  bool
	err error
}

type fakeRuntime struct {
	mu           sync.Mutex
	events       chan string
	ordered      []string
	claimResults []claimResult
	process      func(context.Context, queue.Job) (worker.Result, error)
	terminalErr  error
	reconcileErr error
	initialize   <-chan struct{}
	active       bool
	released     queue.Job
	finalized    string
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
func (runtime *fakeRuntime) Release(_ context.Context, job queue.Job) error {
	runtime.event("release")
	runtime.released = job
	return nil
}
func (runtime *fakeRuntime) QueueDepth(context.Context) (map[queue.State]int64, error) {
	return map[queue.State]int64{}, nil
}
func (runtime *fakeRuntime) Active(context.Context, queue.Job) (bool, error) {
	return runtime.active, nil
}
func (runtime *fakeRuntime) Cancel()      { runtime.event("cancel") }
func (runtime *fakeRuntime) Close() error { runtime.event("close"); return nil }

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
