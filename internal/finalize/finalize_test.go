package finalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/aigate"
	"github.com/nosovk/paperless-ai-ocr/internal/database"
	"github.com/nosovk/paperless-ai-ocr/internal/ocr"
	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/paperlessai"
	"github.com/nosovk/paperless-ai-ocr/internal/pdf"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	"github.com/nosovk/paperless-ai-ocr/internal/worker"
)

var finalizerNow = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

func TestProcessOrdersAndCheckpointsEverySuccessEffect(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationPending)

	if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	want := []string{
		"renew", "stage", "renew", "get-document", "renew", "update-content", "checkpoint:content_updated",
		"renew", "ensure-tag:ai-ocr-complete", "renew", "get-document", "renew", "update-tags:add=11:remove=", "checkpoint:complete_tag_added",
		"renew", "ensure-tag:ai-ocr-failed", "renew", "get-document", "renew", "update-tags:add=:remove=12", "checkpoint:failed_tag_removed",
		"dispatch:reserved", "renew", "dispatch", "dispatch:confirmed", "checkpoint:metadata_dispatched", "renew", "complete",
	}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("trace =\n%q\nwant\n%q", fixture.trace.snapshot(), want)
	}
}

func TestProcessAcceptsReconstructedResultWithoutDownloadHash(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationContentUpdated)
	fixture.result.DownloadSHA256 = ""

	if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if !slices.Contains(fixture.trace.snapshot(), "complete") {
		t.Errorf("trace = %q, want completed finalization", fixture.trace.snapshot())
	}
}

func TestDurableRestartReconstructsAndCompletesFinalization(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := queue.New(db)
	_, created, err := store.Enqueue(queue.EnqueueInput{DocumentID: 42, SourceChecksum: "source-checksum",
		Priority: queue.PriorityWebhook, Model: "model", PromptVersion: ocr.Version})
	if err != nil || !created {
		t.Fatalf("Enqueue() = (%t, %v)", created, err)
	}
	job, ok, err := store.Claim("owner-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %t, %v)", job, ok, err)
	}
	ranges := []queue.BatchRange{{FirstPage: 1, LastPage: 2}}
	if _, err := store.EnsureBatchesContext(context.Background(), job.ID, job.Attempts, job.LeaseOwner, ranges, 200, "png"); err != nil {
		t.Fatal(err)
	}
	canonical := `{"pages":[{"page":1,"text":"one","refused":false},{"page":2,"text":"two","refused":false}]}`
	if err := store.CheckpointBatchContext(context.Background(), job.ID, job.Attempts, job.LeaseOwner, ranges[0], 200, "png", canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireFinalizationContext(context.Background(), job.ID, job.Attempts, job.LeaseOwner, "initial", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceFinalizationContext(context.Background(), job.ID, job.Attempts, job.LeaseOwner, "initial",
		queue.FinalizationPending, queue.FinalizationContentUpdated); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE jobs SET state = 'retry', available_at = '2000-01-01T00:00:00.000000000Z',
		lease_owner = NULL, lease_expires_at = NULL WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	job, ok, err = store.Claim("owner-2", time.Minute)
	if err != nil || !ok {
		t.Fatalf("reclaim = (%+v, %t, %v)", job, ok, err)
	}

	external := &restartPaperless{document: paperless.Document{ID: 42, Checksum: "source-checksum", Content: "already updated", Tags: []int{12}}}
	ocrWorker, err := worker.New(worker.Options{Store: store, Paperless: external, Capability: aigate.PageImages,
		Transcriber: restartTranscriber{}, Inspector: restartInspector{}, Renderer: restartRenderer{}, Model: "model",
		BatchSize: 5, RenderDPI: 200, LeaseDuration: time.Minute, WorkspaceOptions: pdf.WorkspaceOptions{TemporaryByteBudget: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ocrWorker.Process(context.Background(), job)
	if err != nil {
		t.Fatalf("Worker.Process() error = %v", err)
	}
	if result.DownloadSHA256 != "" || !strings.Contains(result.Content, "one") || !strings.Contains(result.Content, "two") {
		t.Fatalf("reconstructed result = %+v", result)
	}
	if external.workerCalls != 0 {
		t.Fatalf("worker external calls = %d, want 0", external.workerCalls)
	}

	dispatcher := &restartDispatcher{}
	finalizer, err := New(Options{Store: store, Paperless: external, Dispatcher: dispatcher,
		LeaseDuration: time.Minute, RetryDelay: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizer.Process(context.Background(), job, result); err != nil {
		t.Fatalf("Finalizer.Process() error = %v", err)
	}
	if dispatcher.calls != 1 || external.contentUpdates != 0 {
		t.Fatalf("dispatch/content updates = %d/%d, want 1/0", dispatcher.calls, external.contentUpdates)
	}
	var state string
	if err := db.QueryRow("SELECT state FROM jobs WHERE id = ?", job.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(queue.StateCompleted) {
		t.Errorf("job state = %q, want completed", state)
	}
}

func TestProcessChecksumMismatchDiscardsOutput(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationPending)
	fixture.paperless.document.Checksum = "changed-secret-checksum"

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryValidation)
	assertRedacted(t, err, fixture.result.SourceChecksum, fixture.paperless.document.Checksum, fixture.result.Content)
	want := []string{"renew", "stage", "renew", "get-document", "fail"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("trace = %q, want %q", fixture.trace.snapshot(), want)
	}
}

func TestProcessRestartRevalidatesChecksumBeforeLaterEffects(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationContentUpdated)
	fixture.paperless.document.Checksum = "changed-secret-checksum"

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryValidation)
	for _, forbidden := range []string{"ensure-tag:ai-ocr-complete", "update-tags:add=11:remove=", "dispatch"} {
		if slices.Contains(fixture.trace.snapshot(), forbidden) {
			t.Errorf("checksum mismatch performed %q: %q", forbidden, fixture.trace.snapshot())
		}
	}
}

func TestProcessContentFailureChangesNoTagsAndSchedulesRetry(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationPending)
	fixture.paperless.updateContentErr = saferr.New(saferr.CategoryPaperless, "content update unavailable")

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryPaperless)
	want := []string{"renew", "stage", "renew", "get-document", "renew", "update-content", "retry"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("trace = %q, want %q", fixture.trace.snapshot(), want)
	}
}

func TestProcessReconcilesAlreadyAppliedContentBeforeCheckpoint(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationPending)
	fixture.paperless.document.Content = fixture.result.Content

	if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if slices.Contains(fixture.trace.snapshot(), "update-content") {
		t.Errorf("trace repeats content PATCH: %q", fixture.trace.snapshot())
	}
	if !slices.Contains(fixture.trace.snapshot(), "checkpoint:content_updated") {
		t.Errorf("trace lacks reconciled content checkpoint: %q", fixture.trace.snapshot())
	}
}

func TestProcessReconcilesAlreadyAppliedTagsBeforeCheckpoint(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationContentUpdated)
	fixture.paperless.document.Tags = []int{7, 11}

	if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if slices.ContainsFunc(fixture.trace.snapshot(), func(value string) bool { return value == "update-tags:add=11:remove=" }) {
		t.Errorf("trace repeats complete-tag mutation: %q", fixture.trace.snapshot())
	}
	if !slices.Contains(fixture.trace.snapshot(), "checkpoint:complete_tag_added") {
		t.Errorf("trace lacks reconciled tag checkpoint: %q", fixture.trace.snapshot())
	}
}

func TestProcessCheckpointFailureReconcilesExternalEffectsOnRetry(t *testing.T) {
	tests := []struct {
		name       string
		checkpoint queue.FinalizationStage
		repeated   string
	}{
		{name: "content", checkpoint: queue.FinalizationContentUpdated, repeated: "update-content"},
		{name: "complete tag", checkpoint: queue.FinalizationCompleteTagAdded, repeated: "update-tags:add=11:remove="},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, queue.FinalizationPending)
			fixture.store.failCheckpoint = test.checkpoint
			err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
			if err == nil {
				t.Fatal("first Process() error = nil")
			}

			fixture.trace.reset()
			fixture.store.failCheckpoint = ""
			fixture.job.Attempts++
			if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
				t.Fatalf("retry Process() error = %v", err)
			}
			if slices.Contains(fixture.trace.snapshot(), test.repeated) {
				t.Errorf("retry repeated confirmed effect %q: %q", test.repeated, fixture.trace.snapshot())
			}
		})
	}
}

func TestProcessConfirmedDispatchCheckpointFailureDoesNotRedispatch(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailedTagRemoved)
	fixture.store.failCheckpoint = queue.FinalizationMetadataDispatched
	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	if err == nil {
		t.Fatal("first Process() error = nil")
	}
	if fixture.store.dispatch != queue.DispatchConfirmed {
		t.Fatalf("dispatch state = %q, want confirmed", fixture.store.dispatch)
	}

	fixture.trace.reset()
	fixture.store.failCheckpoint = ""
	fixture.job.Attempts++
	if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
		t.Fatalf("retry Process() error = %v", err)
	}
	if slices.Contains(fixture.trace.snapshot(), "dispatch") {
		t.Errorf("retry repeated confirmed dispatch: %q", fixture.trace.snapshot())
	}
}

func TestProcessDownstreamFailureResumesWithoutRepeatingConfirmedEffects(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailedTagRemoved)
	fixture.dispatcher.err = saferr.Wrap(saferr.CategoryProvider, "metadata dispatch rejected", retrySafeError{})

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryProvider)
	if got, want := fixture.store.retryAt, finalizerNow.Add(time.Minute); !got.Equal(want) {
		t.Errorf("retry schedule = %s, want %s", got, want)
	}
	want := []string{"renew", "stage", "renew", "get-document", "dispatch:reserved", "renew", "dispatch", "dispatch:none", "retry"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("first trace = %q, want %q", fixture.trace.snapshot(), want)
	}

	fixture.trace.reset()
	fixture.dispatcher.err = nil
	fixture.job.Attempts++
	if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
		t.Fatalf("retry Process() error = %v", err)
	}
	want = []string{"renew", "stage", "renew", "get-document", "dispatch:reserved", "renew", "dispatch", "dispatch:confirmed", "checkpoint:metadata_dispatched", "renew", "complete"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("retry trace = %q, want %q", fixture.trace.snapshot(), want)
	}
}

func TestProcessDoesNotRepeatAmbiguousDispatch(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailedTagRemoved)
	fixture.store.dispatch = queue.DispatchReserved

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryProvider)
	if slices.Contains(fixture.trace.snapshot(), "dispatch") || slices.Contains(fixture.trace.snapshot(), "retry") {
		t.Errorf("ambiguous dispatch was repeated or retried: %q", fixture.trace.snapshot())
	}
}

func TestProcessHTTP500KeepsDispatchReservedAndRequiresReconciliation(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailedTagRemoved)
	fixture.dispatcher.err = saferr.Wrap(saferr.CategoryProvider, "Paperless AI dispatch failed",
		&paperlessai.StatusError{StatusCode: 500})

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryProvider)
	if fixture.store.dispatch != queue.DispatchReserved {
		t.Errorf("dispatch state = %q, want reserved", fixture.store.dispatch)
	}
	if got, want := fixture.store.failedDiagnostic, (queue.SafeDiagnostic{
		Category: saferr.CategoryProvider,
		Message:  uncertainDispatchDiagnostic,
	}); got != want {
		t.Errorf("failed diagnostic = %+v, want %+v", got, want)
	}
	trace := fixture.trace.snapshot()
	if slices.Contains(trace, "dispatch:none") || slices.Contains(trace, "retry") {
		t.Errorf("HTTP 500 cleared reservation or retried: %q", trace)
	}
}

func TestProcessReservedDispatchPreemptsChangedChecksumWithoutPaperlessCall(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailedTagRemoved)
	fixture.store.dispatch = queue.DispatchReserved
	fixture.paperless.document.Checksum = "changed-secret-checksum"

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryProvider)
	if got, want := fixture.store.failedDiagnostic, (queue.SafeDiagnostic{
		Category: saferr.CategoryProvider,
		Message:  uncertainDispatchDiagnostic,
	}); got != want {
		t.Errorf("failed diagnostic = %+v, want %+v", got, want)
	}
	trace := fixture.trace.snapshot()
	if slices.Contains(trace, "get-document") || slices.Contains(trace, "update-content") ||
		slices.ContainsFunc(trace, func(value string) bool {
			return strings.HasPrefix(value, "ensure-tag:") || strings.HasPrefix(value, "update-tags:")
		}) ||
		slices.Contains(trace, "dispatch") {
		t.Errorf("reserved dispatch performed Paperless effect: %q", trace)
	}
	if !slices.Equal(trace, []string{"renew", "stage", "fail"}) {
		t.Errorf("trace = %q, want admission then terminal failure", trace)
	}
}

func TestProcessRestartsAfterLastConfirmedSuccessEffect(t *testing.T) {
	tests := []struct {
		stage queue.FinalizationStage
		want  []string
	}{
		{stage: queue.FinalizationContentUpdated, want: []string{"renew", "stage", "renew", "get-document", "renew", "ensure-tag:ai-ocr-complete", "renew", "get-document", "renew", "update-tags:add=11:remove=", "checkpoint:complete_tag_added", "renew", "ensure-tag:ai-ocr-failed", "renew", "get-document", "renew", "update-tags:add=:remove=12", "checkpoint:failed_tag_removed", "dispatch:reserved", "renew", "dispatch", "dispatch:confirmed", "checkpoint:metadata_dispatched", "renew", "complete"}},
		{stage: queue.FinalizationCompleteTagAdded, want: []string{"renew", "stage", "renew", "get-document", "renew", "ensure-tag:ai-ocr-failed", "renew", "get-document", "renew", "update-tags:add=:remove=12", "checkpoint:failed_tag_removed", "dispatch:reserved", "renew", "dispatch", "dispatch:confirmed", "checkpoint:metadata_dispatched", "renew", "complete"}},
		{stage: queue.FinalizationFailedTagRemoved, want: []string{"renew", "stage", "renew", "get-document", "dispatch:reserved", "renew", "dispatch", "dispatch:confirmed", "checkpoint:metadata_dispatched", "renew", "complete"}},
		{stage: queue.FinalizationMetadataDispatched, want: []string{"renew", "stage", "renew", "get-document", "renew", "complete"}},
	}
	for _, test := range tests {
		t.Run(string(test.stage), func(t *testing.T) {
			fixture := newFixture(t, test.stage)
			if err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result); err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			if !slices.Equal(fixture.trace.snapshot(), test.want) {
				t.Errorf("trace = %q, want %q", fixture.trace.snapshot(), test.want)
			}
		})
	}
}

func TestFailOCRAddsFailureTagBeforeTerminalQueueFailure(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationPending)
	fixture.paperless.document.Tags = []int{7}

	fixture.store.failureCategory = saferr.CategoryProvider
	fixture.store.failureMessage = "OCR processing failed"
	err := fixture.finalizer.FailOCR(context.Background(), fixture.job)
	if err != nil {
		t.Fatalf("FailOCR() error = %v", err)
	}
	want := []string{"renew", "stage", "checkpoint:failure_pending", "renew", "ensure-tag:ai-ocr-failed", "renew", "get-document", "renew", "update-tags:add=12:remove=", "checkpoint:failure_tag_added", "renew", "fail"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("trace = %q, want %q", fixture.trace.snapshot(), want)
	}
}

func TestFailOCRTagFailureResumesFromDurableFailureIntent(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationPending)
	fixture.paperless.document.Tags = []int{7}
	fixture.paperless.updateTagsErr = saferr.New(saferr.CategoryPaperless, "tag update unavailable")

	fixture.store.failureCategory = saferr.CategoryProvider
	fixture.store.failureMessage = "OCR processing failed"
	err := fixture.finalizer.FailOCR(context.Background(), fixture.job)
	assertSafeError(t, err, saferr.CategoryPaperless)
	want := []string{"renew", "stage", "checkpoint:failure_pending", "renew", "ensure-tag:ai-ocr-failed", "renew", "get-document", "renew", "update-tags:add=12:remove=", "retry"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("first trace = %q, want %q", fixture.trace.snapshot(), want)
	}

	fixture.trace.reset()
	fixture.paperless.updateTagsErr = nil
	fixture.job.Attempts++
	if err := fixture.finalizer.FailOCR(context.Background(), fixture.job); err != nil {
		t.Fatalf("retry FailOCR() error = %v", err)
	}
	want = []string{"renew", "stage", "renew", "ensure-tag:ai-ocr-failed", "renew", "get-document", "renew", "update-tags:add=12:remove=", "checkpoint:failure_tag_added", "renew", "fail"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("retry trace = %q, want %q", fixture.trace.snapshot(), want)
	}
}

func TestFailOCRResumesAfterConfirmedFailureTag(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailureTagAdded)

	fixture.store.failureCategory = saferr.CategoryRendering
	fixture.store.failureMessage = "OCR processing failed"
	if err := fixture.finalizer.FailOCR(context.Background(), fixture.job); err != nil {
		t.Fatalf("FailOCR() error = %v", err)
	}
	want := []string{"renew", "stage", "renew", "fail"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("trace = %q, want %q", fixture.trace.snapshot(), want)
	}
}

func TestFinalizerRejectsConcurrentDuplicateAdmission(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailedTagRemoved)
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.dispatcher.block = func() {
		close(started)
		<-release
	}
	first := make(chan error, 1)
	go func() { first <- fixture.finalizer.Process(context.Background(), fixture.job, fixture.result) }()
	<-started

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryValidation)
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Process() error = %v", err)
	}
}

func TestSeparateFinalizersRejectConcurrentDurableAdmission(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailedTagRemoved)
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.dispatcher.block = func() {
		close(started)
		<-release
	}
	first, err := New(Options{Store: fixture.store, Paperless: fixture.paperless, Dispatcher: fixture.dispatcher,
		LeaseDuration: time.Hour, RetryDelay: time.Minute, Now: func() time.Time { return finalizerNow },
		Token: func() (string, error) { return "token-a", nil }})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Options{Store: fixture.store, Paperless: fixture.paperless, Dispatcher: fixture.dispatcher,
		LeaseDuration: time.Hour, RetryDelay: time.Minute, Now: func() time.Time { return finalizerNow },
		Token: func() (string, error) { return "token-b", nil }})
	if err != nil {
		t.Fatal(err)
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- first.Process(context.Background(), fixture.job, fixture.result) }()
	<-started

	err = second.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryValidation)
	if slices.Contains(fixture.trace.snapshot(), "retry") || slices.Contains(fixture.trace.snapshot(), "fail") {
		t.Errorf("duplicate admission changed queue state: %q", fixture.trace.snapshot())
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first Process() error = %v", err)
	}
}

func TestProcessStopsAfterLeaseLoss(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationPending)
	fixture.store.renewErrAt = 3

	err := fixture.finalizer.Process(context.Background(), fixture.job, fixture.result)
	assertSafeError(t, err, saferr.CategoryValidation)
	want := []string{"renew", "stage", "renew", "get-document", "renew"}
	if !slices.Equal(fixture.trace.snapshot(), want) {
		t.Errorf("trace = %q, want %q", fixture.trace.snapshot(), want)
	}
}

func TestProcessHeartbeatDoesNotRaceCompletion(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationMetadataDispatched)
	fixture.finalizer.options.LeaseDuration = 30 * time.Millisecond
	fixture.store.terminalStarted = make(chan struct{})
	fixture.store.releaseTerminal = make(chan struct{})
	result := make(chan error, 1)
	go func() { result <- fixture.finalizer.Process(context.Background(), fixture.job, fixture.result) }()
	<-fixture.store.terminalStarted
	time.Sleep(20 * time.Millisecond)
	close(fixture.store.releaseTerminal)

	if err := <-result; err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if fixture.store.renewDuringTerminal {
		t.Error("heartbeat renewed while completion transition was in progress")
	}
}

func TestFailOCRHeartbeatDoesNotRaceTerminalFailure(t *testing.T) {
	fixture := newFixture(t, queue.FinalizationFailureTagAdded)
	fixture.finalizer.options.LeaseDuration = 30 * time.Millisecond
	fixture.store.failureCategory = saferr.CategoryRendering
	fixture.store.failureMessage = "OCR processing failed"
	fixture.store.terminalStarted = make(chan struct{})
	fixture.store.releaseTerminal = make(chan struct{})
	result := make(chan error, 1)
	go func() { result <- fixture.finalizer.FailOCR(context.Background(), fixture.job) }()
	<-fixture.store.terminalStarted
	time.Sleep(20 * time.Millisecond)
	close(fixture.store.releaseTerminal)

	if err := <-result; err != nil {
		t.Fatalf("FailOCR() error = %v", err)
	}
	if fixture.store.renewDuringTerminal {
		t.Error("heartbeat renewed while failure transition was in progress")
	}
}

type fixture struct {
	finalizer  *Finalizer
	store      *fakeStore
	paperless  *fakePaperless
	dispatcher *fakeDispatcher
	trace      *traceLog
	job        queue.Job
	result     worker.Result
}

func newFixture(t *testing.T, stage queue.FinalizationStage) fixture {
	t.Helper()
	trace := &traceLog{}
	store := &fakeStore{trace: trace, stage: stage, dispatch: queue.DispatchNone, now: finalizerNow}
	paperlessClient := &fakePaperless{trace: trace, document: paperless.Document{ID: 42, Checksum: "source-checksum", Tags: []int{7, 12}}}
	dispatcher := &fakeDispatcher{trace: trace}
	finalizer, err := New(Options{Store: store, Paperless: paperlessClient, Dispatcher: dispatcher,
		LeaseDuration: time.Hour, RetryDelay: time.Minute, Now: func() time.Time { return finalizerNow }})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	job := queue.Job{ID: 9, DocumentID: 42, SourceChecksum: "source-checksum", State: queue.StateProcessing,
		Attempts: 1, LeaseOwner: "owner", LeaseExpiresAt: finalizerNow.Add(time.Hour)}
	result := worker.Result{JobID: 9, DocumentID: 42, SourceChecksum: "source-checksum", DownloadSHA256: "download-hash", Content: "corrected content"}
	return fixture{finalizer: finalizer, store: store, paperless: paperlessClient, dispatcher: dispatcher, trace: trace, job: job, result: result}
}

type traceLog struct {
	mu     sync.Mutex
	values []string
}

func (trace *traceLog) add(value string) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.values = append(trace.values, value)
}

func (trace *traceLog) snapshot() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return slices.Clone(trace.values)
}

func (trace *traceLog) reset() {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.values = nil
}

type fakeStore struct {
	trace               *traceLog
	stage               queue.FinalizationStage
	now                 time.Time
	renewCalls          int
	renewErrAt          int
	dispatch            queue.DispatchState
	failureCategory     saferr.Category
	failureMessage      string
	admissionMu         sync.Mutex
	admissionToken      string
	admissionAttempt    int
	failCheckpoint      queue.FinalizationStage
	failedDiagnostic    queue.SafeDiagnostic
	terminalStarted     chan struct{}
	releaseTerminal     chan struct{}
	terminalMu          sync.Mutex
	terminalActive      bool
	renewDuringTerminal bool
	retryAt             time.Time
}

func (store *fakeStore) RenewLeaseContext(context.Context, int64, int, string, time.Duration) error {
	store.trace.add("renew")
	store.terminalMu.Lock()
	if store.terminalActive {
		store.renewDuringTerminal = true
		store.terminalMu.Unlock()
		return saferr.New(saferr.CategoryValidation, "lease cleared by terminal transition")
	}
	store.terminalMu.Unlock()
	store.renewCalls++
	if store.renewCalls == store.renewErrAt {
		return saferr.New(saferr.CategoryValidation, "stale lease")
	}
	return nil
}

func (*fakeStore) RenewFinalizationContext(context.Context, int64, int, string, string, time.Duration) error {
	return nil
}

func (store *fakeStore) AcquireFinalizationContext(_ context.Context, _ int64, attempt int, _ string, token string, _ time.Duration) (queue.FinalizationState, error) {
	store.trace.add("stage")
	store.admissionMu.Lock()
	defer store.admissionMu.Unlock()
	if store.admissionAttempt == attempt && store.admissionToken != "" && store.admissionToken != token {
		return queue.FinalizationState{}, saferr.New(saferr.CategoryValidation, "finalizer is already active for this job")
	}
	store.admissionToken = token
	store.admissionAttempt = attempt
	return queue.FinalizationState{Stage: store.stage, Dispatch: store.dispatch, FailureCategory: store.failureCategory, FailureMessage: store.failureMessage}, nil
}

func (store *fakeStore) FinalizationStateContext(context.Context, int64, int, string, string) (queue.FinalizationState, error) {
	return queue.FinalizationState{Stage: store.stage, Dispatch: store.dispatch, FailureCategory: store.failureCategory, FailureMessage: store.failureMessage}, nil
}

func (store *fakeStore) AdvanceFinalizationContext(_ context.Context, _ int64, _ int, _ string, _ string, _, to queue.FinalizationStage) error {
	store.trace.add("checkpoint:" + string(to))
	if store.failCheckpoint == to {
		return saferr.New(saferr.CategoryInternal, "checkpoint unavailable")
	}
	store.stage = to
	return nil
}

func (store *fakeStore) SetDispatchStateContext(_ context.Context, _ int64, _ int, _ string, _ string, _, to queue.DispatchState) error {
	store.trace.add("dispatch:" + string(to))
	store.dispatch = to
	return nil
}

func (store *fakeStore) CompleteContext(context.Context, int64, int, string) error {
	store.trace.add("complete")
	store.blockTerminal()
	return nil
}

func (store *fakeStore) FailContext(_ context.Context, _ int64, _ int, _ string, diagnostic queue.SafeDiagnostic) error {
	store.trace.add("fail")
	store.failedDiagnostic = diagnostic
	store.blockTerminal()
	return nil
}

func (store *fakeStore) blockTerminal() {
	if store.terminalStarted == nil {
		return
	}
	store.terminalMu.Lock()
	store.terminalActive = true
	store.terminalMu.Unlock()
	close(store.terminalStarted)
	<-store.releaseTerminal
	store.terminalMu.Lock()
	store.terminalActive = false
	store.terminalMu.Unlock()
}

func (store *fakeStore) ScheduleRetryContext(_ context.Context, _ int64, _ int, _ string, retryAt time.Time, _ queue.SafeDiagnostic) error {
	store.trace.add("retry")
	store.retryAt = retryAt
	return nil
}

type fakePaperless struct {
	trace            *traceLog
	document         paperless.Document
	updateContentErr error
	updateTagsErr    error
}

func (client *fakePaperless) GetDocument(context.Context, int) (paperless.Document, error) {
	client.trace.add("get-document")
	return client.document, nil
}

func (client *fakePaperless) UpdateContent(_ context.Context, _ int, content string) error {
	client.trace.add("update-content")
	if client.updateContentErr == nil {
		client.document.Content = content
	}
	return client.updateContentErr
}

func (client *fakePaperless) EnsureTag(_ context.Context, name string) (paperless.Tag, error) {
	client.trace.add("ensure-tag:" + name)
	if name == "ai-ocr-complete" {
		return paperless.Tag{ID: 11, Name: name}, nil
	}
	return paperless.Tag{ID: 12, Name: name}, nil
}

func (client *fakePaperless) UpdateTags(_ context.Context, _ int, current, add, remove []int) error {
	client.trace.add(fmt.Sprintf("update-tags:add=%s:remove=%s", ids(add), ids(remove)))
	if client.updateTagsErr != nil {
		return client.updateTagsErr
	}
	result := slices.Clone(current)
	for _, removeID := range remove {
		result = slices.DeleteFunc(result, func(id int) bool { return id == removeID })
	}
	for _, addID := range add {
		if !slices.Contains(result, addID) {
			result = append(result, addID)
		}
	}
	client.document.Tags = result
	return nil
}

type fakeDispatcher struct {
	trace *traceLog
	err   error
	block func()
}

func (dispatcher *fakeDispatcher) Dispatch(context.Context, int) error {
	dispatcher.trace.add("dispatch")
	if dispatcher.block != nil {
		dispatcher.block()
	}
	return dispatcher.err
}

func ids(values []int) string {
	if len(values) == 0 {
		return ""
	}
	return fmt.Sprint(values[0])
}

type retrySafeError struct{}

func (retrySafeError) Error() string   { return "confirmed rejection" }
func (retrySafeError) RetrySafe() bool { return true }

type restartPaperless struct {
	document       paperless.Document
	workerCalls    int
	contentUpdates int
}

func (client *restartPaperless) GetDocument(_ context.Context, _ int) (paperless.Document, error) {
	return client.document, nil
}

func (client *restartPaperless) DownloadOriginal(context.Context, int, io.Writer) error {
	client.workerCalls++
	return errors.New("unexpected download")
}

func (client *restartPaperless) UpdateContent(context.Context, int, string) error {
	client.contentUpdates++
	return nil
}

func (*restartPaperless) EnsureTag(_ context.Context, name string) (paperless.Tag, error) {
	if name == completeTagName {
		return paperless.Tag{ID: 11, Name: name}, nil
	}
	return paperless.Tag{ID: 12, Name: name}, nil
}

func (client *restartPaperless) UpdateTags(_ context.Context, _ int, current, add, remove []int) error {
	tags := slices.Clone(current)
	for _, id := range remove {
		tags = slices.DeleteFunc(tags, func(current int) bool { return current == id })
	}
	for _, id := range add {
		if !slices.Contains(tags, id) {
			tags = append(tags, id)
		}
	}
	client.document.Tags = tags
	return nil
}

type restartTranscriber struct{}

func (restartTranscriber) Transcribe(context.Context, aigate.Transcription) (json.RawMessage, error) {
	return nil, errors.New("unexpected transcription")
}

type restartInspector struct{}

func (restartInspector) Inspect(context.Context, *pdf.Workspace, string) (pdf.Info, error) {
	return pdf.Info{}, errors.New("unexpected inspection")
}

type restartRenderer struct{}

func (restartRenderer) Render(context.Context, *pdf.Workspace, string, int, int, func([]pdf.Page) error) error {
	return errors.New("unexpected rendering")
}

type restartDispatcher struct{ calls int }

func (dispatcher *restartDispatcher) Dispatch(context.Context, int) error {
	dispatcher.calls++
	return nil
}

func assertSafeError(t *testing.T, err error, category saferr.Category) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil")
	}
	var safeErr *saferr.Error
	if !errors.As(err, &safeErr) || safeErr.Category() != category {
		t.Fatalf("error = %v, want category %q", err, category)
	}
}

func assertRedacted(t *testing.T, err error, canaries ...string) {
	t.Helper()
	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		formatted := fmt.Sprintf(format, err)
		for _, canary := range canaries {
			if canary != "" && slices.ContainsFunc([]string{formatted}, func(value string) bool { return len(value) >= len(canary) && contains(value, canary) }) {
				t.Errorf("format %s disclosed %q: %q", format, canary, formatted)
			}
		}
	}
}

func contains(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
