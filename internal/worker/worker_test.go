package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/aigate"
	"github.com/nosovk/paperless-ai-ocr/internal/ocr"
	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/pdf"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

func TestProcessDirectPDFAndLeavesJobForFinalizer(t *testing.T) {
	fixture := newFixture(t, 3, aigate.DirectPDF)
	result, err := fixture.worker.Process(context.Background(), fixture.job)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if fixture.transcriber.calls != 1 || fixture.transcriber.inputs[0].Capability != aigate.DirectPDF || fixture.renderer.calls != 0 {
		t.Errorf("direct calls/render = %d/%d, input = %+v", fixture.transcriber.calls, fixture.renderer.calls, fixture.transcriber.inputs)
	}
	if len(fixture.store.batches) != 1 || fixture.store.batches[0].FirstPage != 1 || fixture.store.batches[0].LastPage != 3 || fixture.store.failed || fixture.store.completed {
		t.Errorf("checkpoint/final state = %+v failed=%t completed=%t", fixture.store.batches, fixture.store.failed, fixture.store.completed)
	}
	if result.JobID != fixture.job.ID || result.DocumentID != fixture.job.DocumentID || result.SourceChecksum != fixture.job.SourceChecksum || result.DownloadSHA256 == "" || !strings.Contains(result.Content, "PAGE 3") {
		t.Errorf("Process() result = %+v", result)
	}
}

func TestProcessUsesImagesForOversizedOrLongPDF(t *testing.T) {
	for _, test := range []struct {
		name  string
		pages int
		pdf   []byte
		want  int
	}{
		{name: "oversized", pages: 2, pdf: make([]byte, (8<<20)+1), want: 1},
		{name: "long", pages: 11, pdf: []byte("pdf"), want: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, test.pages, aigate.DirectPDF)
			fixture.paperless.pdf = test.pdf
			if _, err := fixture.worker.Process(context.Background(), fixture.job); err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			if fixture.transcriber.calls != test.want || fixture.renderer.calls != test.want {
				t.Errorf("model/render calls = %d/%d, want %d", fixture.transcriber.calls, fixture.renderer.calls, test.want)
			}
			for _, input := range fixture.transcriber.inputs {
				if input.Capability != aigate.PageImages || len(input.PDF) != 0 || len(input.Images) > 5 {
					t.Errorf("image fallback input = %+v", input)
				}
			}
		})
	}
}

func TestProcessFallsBackOnlyForUnsupportedDirectAttachment(t *testing.T) {
	fixture := newFixture(t, 2, aigate.DirectPDF)
	fixture.transcriber.errors = []error{unsupportedError{}, nil}
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(fixture.transcriber.inputs) != 2 || fixture.transcriber.inputs[0].Capability != aigate.DirectPDF || fixture.transcriber.inputs[1].Capability != aigate.PageImages {
		t.Errorf("fallback inputs = %+v", fixture.transcriber.inputs)
	}

	fixture = newFixture(t, 2, aigate.DirectPDF)
	fixture.transcriber.errors = []error{saferr.New(saferr.CategoryProvider, "transcription authentication failed")}
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err == nil || fixture.renderer.calls != 0 || !fixture.store.failed {
		t.Fatalf("permanent direct failure = err %v render %d failed %t", err, fixture.renderer.calls, fixture.store.failed)
	}
}

func TestProcessRestartRevalidatesCanonicalCheckpointAndSkipsWork(t *testing.T) {
	fixture := newFixture(t, 6, aigate.PageImages)
	fixture.store.batches = []queue.Batch{
		{JobID: fixture.job.ID, FirstPage: 1, LastPage: 5, RenderDPI: 200, RenderFormat: "png", State: queue.StateCompleted, ResultText: rawPages(1, 5)},
		{JobID: fixture.job.ID, FirstPage: 6, LastPage: 6, RenderDPI: 200, RenderFormat: "png", State: queue.StatePending},
	}
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if fixture.renderer.calls != 1 || fixture.transcriber.calls != 1 {
		t.Errorf("restart render/model calls = %d/%d, want 1/1", fixture.renderer.calls, fixture.transcriber.calls)
	}

	fixture = newFixture(t, 1, aigate.PageImages)
	fixture.store.batches = []queue.Batch{{JobID: fixture.job.ID, FirstPage: 1, LastPage: 1, RenderDPI: 200, RenderFormat: "png", State: queue.StateCompleted, ResultText: `{"pages":[]}`}}
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err == nil || fixture.transcriber.calls != 0 || !fixture.store.failed {
		t.Fatalf("invalid checkpoint = err %v model %d failed %t", err, fixture.transcriber.calls, fixture.store.failed)
	}
}

func TestProcessRejectsIdentityAndChecksumBeforeModel(t *testing.T) {
	for _, mutate := range []func(*fixture){
		func(f *fixture) { f.job.Model = "other" },
		func(f *fixture) { f.job.PromptVersion = "other" },
		func(f *fixture) { f.paperless.document.Checksum = "changed" },
	} {
		fixture := newFixture(t, 1, aigate.PageImages)
		mutate(fixture)
		if _, err := fixture.worker.Process(context.Background(), fixture.job); err == nil || fixture.transcriber.calls != 0 || fixture.renderer.calls != 0 {
			t.Fatalf("identity mismatch = err %v model/render %d/%d", err, fixture.transcriber.calls, fixture.renderer.calls)
		}
	}
}

func TestProcessRetriesModelWithRetryAfterAndExhausts(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	fixture.transcriber.errors = []error{retryableError{delay: 7 * time.Second}, nil}
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if fmt.Sprint(fixture.sleeps) != "[7s]" || fixture.transcriber.maxActive != 1 {
		t.Errorf("sleeps/max active = %v/%d", fixture.sleeps, fixture.transcriber.maxActive)
	}

	fixture = newFixture(t, 1, aigate.PageImages)
	fixture.transcriber.errors = []error{retryableError{}, retryableError{}, retryableError{}}
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err == nil || !fixture.store.failed || fixture.transcriber.calls != 3 {
		t.Fatalf("exhaustion = err %v failed %t calls %d", err, fixture.store.failed, fixture.transcriber.calls)
	}
	if fixture.store.diagnostic.Message != "OCR processing failed" {
		t.Errorf("diagnostic = %+v", fixture.store.diagnostic)
	}
}

func TestNewBoundsModelAttempts(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	options := fixture.worker.options

	for _, test := range []struct {
		name  string
		value int
		want  int
	}{
		{name: "default", value: 0, want: defaultModelAttempts},
		{name: "minimum", value: 1, want: 1},
		{name: "maximum", value: 10, want: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			options.ModelAttempts = test.value
			worker, err := New(options)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if worker.options.ModelAttempts != test.want {
				t.Errorf("ModelAttempts = %d, want %d", worker.options.ModelAttempts, test.want)
			}
		})
	}

	for _, value := range []int{-1, 11, math.MaxInt} {
		t.Run(strconv.Itoa(value), func(t *testing.T) {
			options.ModelAttempts = value
			if _, err := New(options); err == nil {
				t.Fatalf("New(ModelAttempts: %d) error = nil", value)
			}
		})
	}
}

func TestBackoffDelayGrowsAndSaturates(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 5, want: 16 * time.Second},
		{attempt: 6, want: maximumBackoff},
		{attempt: 10, want: maximumBackoff},
		{attempt: strconv.IntSize - 1, want: maximumBackoff},
		{attempt: strconv.IntSize, want: maximumBackoff},
		{attempt: math.MaxInt, want: maximumBackoff},
	}

	previous := time.Duration(0)
	for _, test := range tests {
		delay := backoffDelay(test.attempt, func(time.Duration) time.Duration { return 0 })
		if delay != test.want {
			t.Errorf("backoffDelay(%d) = %s, want %s", test.attempt, delay, test.want)
		}
		if delay <= 0 {
			t.Errorf("backoffDelay(%d) = %s, want positive", test.attempt, delay)
		}
		if delay < previous {
			t.Errorf("backoffDelay(%d) = %s, want at least %s", test.attempt, delay, previous)
		}
		previous = delay
	}
}

func TestBackoffDelayClampsJitter(t *testing.T) {
	if got := backoffDelay(1, func(time.Duration) time.Duration { return time.Duration(math.MaxInt64) }); got != maximumBackoff {
		t.Errorf("backoffDelay() with large positive jitter = %s, want %s", got, maximumBackoff)
	}
	if got := backoffDelay(1, func(time.Duration) time.Duration { return time.Duration(math.MinInt64) }); got != defaultBackoff {
		t.Errorf("backoffDelay() with negative jitter = %s, want %s", got, defaultBackoff)
	}
}

func TestProcessCancellationSchedulesRetryAndLostLeaseStopsWork(t *testing.T) {
	fixture := newFixture(t, 6, aigate.PageImages)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.transcriber.after = cancel
	if _, err := fixture.worker.Process(ctx, fixture.job); !errors.Is(err, context.Canceled) || !fixture.store.retried || fixture.store.failed || fixture.transcriber.calls != 1 {
		t.Fatalf("cancellation = err %v retry %t failed %t calls %d", err, fixture.store.retried, fixture.store.failed, fixture.transcriber.calls)
	}

	fixture = newFixture(t, 6, aigate.PageImages)
	fixture.store.renewErrAt = 3
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err == nil || fixture.transcriber.calls > 1 || len(fixture.store.batches) > 0 && fixture.store.batches[len(fixture.store.batches)-1].State == queue.StateCompleted {
		t.Fatalf("lost lease = err %v calls %d batches %+v", err, fixture.transcriber.calls, fixture.store.batches)
	}
}

func TestProcessEnforcesDocumentDeadline(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	fixture.worker.options.DocumentDeadline = time.Nanosecond
	if _, err := fixture.worker.Process(context.Background(), fixture.job); !errors.Is(err, context.DeadlineExceeded) || !fixture.store.retried || fixture.store.failed {
		t.Fatalf("deadline = err %v retry %t failed %t", err, fixture.store.retried, fixture.store.failed)
	}
}

func TestProcessRenewsLeaseDuringLongModelRequest(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	fixture.worker.options.LeaseDuration = 20 * time.Millisecond
	release := make(chan struct{})
	fixture.transcriber.wait = release
	result := make(chan error, 1)
	go func() {
		_, err := fixture.worker.Process(context.Background(), fixture.job)
		result <- err
	}()
	deadline := time.Now().Add(time.Second)
	for fixture.store.renewCount() < 4 {
		if time.Now().After(deadline) {
			t.Fatal("lease heartbeat did not renew during model request")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("Process() error = %v", err)
	}
}

func TestHeartbeatShutdownIgnoresInFlightRenewalCancellation(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	fixture.worker.options.LeaseDuration = 3 * time.Millisecond
	fixture.store.blockRenewAt = 1
	fixture.store.renewStarted = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go fixture.worker.heartbeat(ctx, fixture.job, cancel, done)
	<-fixture.store.renewStarted
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("heartbeat shutdown error = %v, want nil", err)
	}
}

func TestHeartbeatGenuineRenewalFailureCancelsProcessing(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	fixture.worker.options.LeaseDuration = 3 * time.Millisecond
	fixture.store.renewErrAt = 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go fixture.worker.heartbeat(ctx, fixture.job, cancel, done)
	err := <-done
	var lostLease *lostLeaseError
	if !errors.As(err, &lostLease) || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("heartbeat = %v, context = %v, want lost lease and canceled processing", err, ctx.Err())
	}
}

func TestProcessShutdownPreservesSuccessCancellationAndDeadline(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*fixture) context.Context
		want  error
	}{
		{name: "success", setup: func(*fixture) context.Context { return context.Background() }},
		{name: "caller cancel", setup: func(fixture *fixture) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			fixture.transcriber.after = cancel
			return ctx
		}, want: context.Canceled},
		{name: "deadline", setup: func(fixture *fixture) context.Context {
			fixture.worker.options.DocumentDeadline = time.Nanosecond
			return context.Background()
		}, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, 1, aigate.PageImages)
			fixture.worker.options.LeaseDuration = time.Nanosecond
			result, err := fixture.worker.Process(test.setup(fixture), fixture.job)
			if test.want == nil && (err != nil || result.Content == "") {
				t.Fatalf("Process() = (%+v, %v), want success", result, err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Process() error = %v, want %v", err, test.want)
			}
			if errors.As(err, new(*lostLeaseError)) {
				t.Fatalf("shutdown replaced process outcome: %v", err)
			}
		})
	}
}

func TestProcessCleansWorkspaceAndRenderedImagesOnFailure(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	fixture.transcriber.errors = []error{saferr.New(saferr.CategoryProvider, "permanent")}
	if _, err := fixture.worker.Process(context.Background(), fixture.job); err == nil {
		t.Fatal("Process() error = nil")
	}
	if fixture.workspacePath == "" {
		t.Fatal("workspace was not created")
	}
	if _, err := os.Stat(fixture.workspacePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace still exists: %v", err)
	}
	for _, path := range fixture.renderer.paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("rendered image still exists: %v", err)
		}
	}
	if fixture.paperless.updated {
		t.Error("worker mutated Paperless content")
	}
}

func TestProcessDoesNotDiscloseDependencyErrors(t *testing.T) {
	fixture := newFixture(t, 1, aigate.PageImages)
	fixture.transcriber.errors = []error{errors.New("CANARY document bytes model URL secret")}
	_, err := fixture.worker.Process(context.Background(), fixture.job)
	if err == nil {
		t.Fatal("Process() error = nil")
	}
	formatted := fmt.Sprintf("%s|%q|%v|%+v|%#v", err, err, err, err, err)
	if strings.Contains(formatted, "CANARY") || strings.Contains(formatted, "document bytes") {
		t.Fatalf("Process() error disclosed dependency data: %s", formatted)
	}
}

func TestProcessReturnsTerminalTransitionFailure(t *testing.T) {
	for _, transitionErr := range []error{
		saferr.New(saferr.CategoryValidation, "stale lease"),
		errors.New("CANARY database path secret"),
	} {
		fixture := newFixture(t, 1, aigate.PageImages)
		fixture.transcriber.errors = []error{saferr.New(saferr.CategoryProvider, "provider rejected request")}
		fixture.store.failErr = transitionErr
		_, err := fixture.worker.Process(context.Background(), fixture.job)
		if err == nil || fixture.store.failed {
			t.Fatalf("Process() = %v, failed=%t, want transition failure", err, fixture.store.failed)
		}
		if err.Error() == "provider: provider rejected request" {
			t.Fatalf("Process() returned processing error instead of transition failure: %v", err)
		}
		if strings.Contains(fmt.Sprintf("%v|%+v|%#v", err, err, err), "CANARY") {
			t.Fatalf("transition error disclosed database details: %v", err)
		}
	}
}

func TestProcessReturnsRetryTransitionFailureWithoutContextSentinel(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "stale", err: saferr.New(saferr.CategoryValidation, "stale lease")},
		{name: "database", err: errors.New("CANARY sqlite database secret")},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, 1, aigate.PageImages)
			fixture.store.retryErr = test.err
			ctx, cancel := context.WithCancel(context.Background())
			fixture.transcriber.after = cancel
			_, err := fixture.worker.Process(ctx, fixture.job)
			if err == nil || fixture.store.retried || errors.Is(err, context.Canceled) {
				t.Fatalf("Process() = %v, retried=%t, want transition failure without context sentinel", err, fixture.store.retried)
			}
			if strings.Contains(fmt.Sprintf("%v|%+v|%#v", err, err, err), "CANARY") {
				t.Fatalf("transition error disclosed database details: %v", err)
			}
		})
	}
}

func TestPlanRangesBoundsPageCountWithoutOverflow(t *testing.T) {
	ranges, err := planRanges(maxDocumentPages, 5, aigate.PageImages)
	if err != nil || len(ranges) != maxDocumentPages/5 || ranges[len(ranges)-1].LastPage != maxDocumentPages {
		t.Fatalf("planRanges(max) = (%d ranges, %v)", len(ranges), err)
	}
	for _, pages := range []int{0, -1, maxDocumentPages + 1, math.MaxInt} {
		if _, err := planRanges(pages, 5, aigate.PageImages); errorCategory(err) != saferr.CategoryRendering {
			t.Fatalf("planRanges(%d) error = %v, want rendering", pages, err)
		}
	}
}

func TestProcessRejectsExcessiveInspectorPageCountBeforeBatchesOrModel(t *testing.T) {
	for _, pages := range []int{maxDocumentPages + 1, math.MaxInt} {
		fixture := newFixture(t, pages, aigate.PageImages)
		_, err := fixture.worker.Process(context.Background(), fixture.job)
		if errorCategory(err) != saferr.CategoryRendering || fixture.transcriber.calls != 0 || fixture.renderer.calls != 0 || len(fixture.store.batches) != 0 {
			t.Fatalf("Process(%d pages) = err %v, model/render/batches %d/%d/%d", pages, err, fixture.transcriber.calls, fixture.renderer.calls, len(fixture.store.batches))
		}
	}
}

type fixture struct {
	worker        *Worker
	job           queue.Job
	store         *fakeStore
	paperless     *fakePaperless
	transcriber   *fakeTranscriber
	renderer      *fakeRenderer
	sleeps        []time.Duration
	workspacePath string
}

func newFixture(t *testing.T, pages int, capability aigate.Capability) *fixture {
	t.Helper()
	job := queue.Job{ID: 9, DocumentID: 42, SourceChecksum: "opaque", State: queue.StateProcessing, Attempts: 1, LeaseOwner: "worker", Model: "model", PromptVersion: ocr.Version, LeaseExpiresAt: time.Now().Add(time.Hour)}
	store := &fakeStore{job: job}
	paperlessClient := &fakePaperless{document: paperless.Document{ID: 42, Content: "native draft", Checksum: "opaque"}, pdf: []byte("%PDF fixture")}
	transcriber := &fakeTranscriber{}
	renderer := &fakeRenderer{t: t}
	result := &fixture{job: job, store: store, paperless: paperlessClient, transcriber: transcriber, renderer: renderer}
	worker, err := New(Options{
		Store: store, Paperless: paperlessClient, Capability: capability, Transcriber: transcriber,
		WorkspaceOptions: pdf.WorkspaceOptions{TemporaryByteBudget: 64 << 20},
		WorkspaceFactory: func(ctx context.Context, jobID int64, options pdf.WorkspaceOptions) (*pdf.Workspace, error) {
			workspace, err := pdf.NewWorkspace(ctx, jobID, options)
			if err == nil {
				path, _ := workspace.Path("probe")
				result.workspacePath = strings.TrimSuffix(path, "/probe")
			}
			return workspace, err
		},
		Inspector: inspectorFunc(func(context.Context, *pdf.Workspace, string) (pdf.Info, error) { return pdf.Info{Pages: pages}, nil }),
		Renderer:  renderer, Model: "model", BatchSize: 5, RenderDPI: 200, ModelAttempts: 3,
		LeaseDuration: time.Minute, RetryDelay: time.Minute, DocumentDeadline: time.Hour,
		Sleep: func(ctx context.Context, delay time.Duration) error {
			result.sleeps = append(result.sleeps, delay)
			return ctx.Err()
		},
		Jitter: func(time.Duration) time.Duration { return 0 },
		Retry: func(err error) (aigate.RetryClass, time.Duration, bool) {
			var retry retryableError
			if errors.As(err, &retry) {
				return aigate.RetryUnavailable, retry.delay, true
			}
			return aigate.Retry(err)
		},
		Unsupported: func(err error) bool {
			var unsupported unsupportedError
			return errors.As(err, &unsupported) || aigate.UnsupportedAttachment(err)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result.worker = worker
	return result
}

type fakeStore struct {
	mu           sync.Mutex
	job          queue.Job
	batches      []queue.Batch
	renews       int
	renewErrAt   int
	failed       bool
	completed    bool
	retried      bool
	diagnostic   queue.SafeDiagnostic
	blockRenewAt int
	renewStarted chan struct{}
	failErr      error
	retryErr     error
}

func (store *fakeStore) RenewLeaseContext(ctx context.Context, _ int64, _ int, _ string, _ time.Duration) error {
	store.mu.Lock()
	store.renews++
	if store.renewStarted != nil {
		select {
		case <-store.renewStarted:
		default:
			close(store.renewStarted)
		}
	}
	block := store.blockRenewAt != 0 && store.renews == store.blockRenewAt
	renewErrAt := store.renewErrAt
	renames := store.renews
	store.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if renewErrAt != 0 && renames >= renewErrAt {
		return saferr.New(saferr.CategoryValidation, "stale lease")
	}
	return nil
}
func (store *fakeStore) renewCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.renews
}
func (store *fakeStore) EnsureBatchesContext(_ context.Context, jobID int64, _ int, _ string, ranges []queue.BatchRange, dpi int, format string) ([]queue.Batch, error) {
	if len(store.batches) == 0 {
		for _, pageRange := range ranges {
			store.batches = append(store.batches, queue.Batch{JobID: jobID, FirstPage: pageRange.FirstPage, LastPage: pageRange.LastPage, RenderDPI: dpi, RenderFormat: format, State: queue.StatePending})
		}
		return append([]queue.Batch(nil), store.batches...), nil
	}
	if len(store.batches) != len(ranges) {
		return nil, saferr.New(saferr.CategoryValidation, "incompatible checkpoints")
	}
	for index, batch := range store.batches {
		if batch.FirstPage != ranges[index].FirstPage || batch.LastPage != ranges[index].LastPage || batch.RenderDPI != dpi || batch.RenderFormat != format {
			return nil, saferr.New(saferr.CategoryValidation, "incompatible checkpoints")
		}
	}
	return append([]queue.Batch(nil), store.batches...), nil
}
func (store *fakeStore) ListBatchesContext(context.Context, int64, int, string) ([]queue.Batch, error) {
	return append([]queue.Batch(nil), store.batches...), nil
}
func (store *fakeStore) CheckpointBatchContext(_ context.Context, _ int64, _ int, _ string, pageRange queue.BatchRange, _ int, _ string, result string) error {
	for index := range store.batches {
		if store.batches[index].FirstPage == pageRange.FirstPage && store.batches[index].LastPage == pageRange.LastPage {
			store.batches[index].State = queue.StateCompleted
			store.batches[index].ResultText = result
			return nil
		}
	}
	return errors.New("missing batch")
}
func (store *fakeStore) FailContext(_ context.Context, _ int64, _ int, _ string, diagnostic queue.SafeDiagnostic) error {
	if store.failErr != nil {
		return store.failErr
	}
	store.failed, store.diagnostic = true, diagnostic
	return nil
}
func (store *fakeStore) ScheduleRetryContext(context.Context, int64, int, string, time.Time, queue.SafeDiagnostic) error {
	if store.retryErr != nil {
		return store.retryErr
	}
	store.retried = true
	return nil
}

type fakePaperless struct {
	document paperless.Document
	pdf      []byte
	updated  bool
}

func (client *fakePaperless) GetDocument(context.Context, int) (paperless.Document, error) {
	return client.document, nil
}
func (client *fakePaperless) DownloadOriginal(_ context.Context, _ int, destination io.Writer) error {
	_, err := destination.Write(client.pdf)
	return err
}

type inspectorFunc func(context.Context, *pdf.Workspace, string) (pdf.Info, error)

func (fn inspectorFunc) Inspect(ctx context.Context, workspace *pdf.Workspace, name string) (pdf.Info, error) {
	return fn(ctx, workspace, name)
}

type fakeRenderer struct {
	t     *testing.T
	calls int
	paths []string
}

func (renderer *fakeRenderer) Render(_ context.Context, _ *pdf.Workspace, _ string, firstPage, lastPage int, visit func([]pdf.Page) error) error {
	renderer.calls++
	dir := renderer.t.TempDir()
	pages := make([]pdf.Page, 0, lastPage-firstPage+1)
	for page := firstPage; page <= lastPage; page++ {
		path := fmt.Sprintf("%s/page-%d.png", dir, page)
		if err := os.WriteFile(path, append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, byte(page)), 0o600); err != nil {
			return err
		}
		renderer.paths = append(renderer.paths, path)
		pages = append(pages, pdf.Page{Number: page, Path: path, Size: 9})
	}
	err := visit(pages)
	for _, page := range pages {
		_ = os.Remove(page.Path)
	}
	return err
}

type fakeTranscriber struct {
	mu                       sync.Mutex
	calls, active, maxActive int
	inputs                   []aigate.Transcription
	errors                   []error
	after                    func()
	wait                     <-chan struct{}
	waitContext              bool
}

func (transcriber *fakeTranscriber) Transcribe(ctx context.Context, input aigate.Transcription) (json.RawMessage, error) {
	transcriber.mu.Lock()
	transcriber.calls++
	transcriber.active++
	transcriber.maxActive = max(transcriber.maxActive, transcriber.active)
	call := transcriber.calls
	transcriber.inputs = append(transcriber.inputs, input)
	transcriber.mu.Unlock()
	defer func() { transcriber.mu.Lock(); transcriber.active--; transcriber.mu.Unlock() }()
	if transcriber.after != nil {
		transcriber.after()
	}
	if transcriber.wait != nil {
		<-transcriber.wait
	}
	if transcriber.waitContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if call <= len(transcriber.errors) && transcriber.errors[call-1] != nil {
		return nil, transcriber.errors[call-1]
	}
	return json.RawMessage(rawPages(input.FirstPage, input.LastPage)), nil
}

func rawPages(firstPage, lastPage int) string {
	pages := make([]map[string]any, 0, lastPage-firstPage+1)
	for page := firstPage; page <= lastPage; page++ {
		pages = append(pages, map[string]any{"page": page, "text": fmt.Sprintf("page %d", page), "refused": false})
	}
	raw, _ := json.Marshal(map[string]any{"pages": pages})
	return string(raw)
}

func errorCategory(err error) saferr.Category {
	var safeError *saferr.Error
	if errors.As(err, &safeError) {
		return safeError.Category()
	}
	return ""
}

type retryableError struct{ delay time.Duration }

func (retryableError) Error() string { return "provider unavailable" }

type unsupportedError struct{}

func (unsupportedError) Error() string { return "unsupported attachment" }
