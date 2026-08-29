package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/database"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

var testNow = time.Date(2026, time.August, 29, 12, 0, 0, 123456789, time.FixedZone("test", 2*60*60))

func TestEnqueueCandidateIsDurableIdempotentAndPromotes(t *testing.T) {
	now := testNow
	path := filepath.Join(t.TempDir(), "queue.db")
	q := openTestQueue(t, path, now)

	created, err := q.EnqueueCandidate(context.Background(), 42, PriorityBackfill)
	if err != nil || !created {
		t.Fatalf("first EnqueueCandidate() = (%t, %v), want (true, nil)", created, err)
	}
	now = now.Add(time.Second)
	q.now = func() time.Time { return now }
	created, err = q.EnqueueCandidate(context.Background(), 42, PriorityWebhook)
	if err != nil || created {
		t.Fatalf("promoting EnqueueCandidate() = (%t, %v), want (false, nil)", created, err)
	}
	created, err = q.EnqueueCandidate(context.Background(), 42, PriorityBackfill)
	if err != nil || created {
		t.Fatalf("duplicate EnqueueCandidate() = (%t, %v), want (false, nil)", created, err)
	}
	if err := q.db.Close(); err != nil {
		t.Fatalf("close candidate database: %v", err)
	}
	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("reopen candidate database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var count int
	var priority Priority
	var createdAt, updatedAt string
	if err := db.QueryRow(`SELECT count(*), priority, created_at, updated_at
		FROM candidates WHERE document_id = 42`).Scan(&count, &priority, &createdAt, &updatedAt); err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if count != 1 || priority != PriorityWebhook {
		t.Errorf("candidate = (count %d, priority %d), want (1, %d)", count, priority, PriorityWebhook)
	}
	if createdAt == updatedAt {
		t.Errorf("candidate timestamps = (%q, %q), want promotion to advance updated_at", createdAt, updatedAt)
	}
}

func TestEnqueueCandidateValidatesInputAndContext(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	for _, test := range []struct {
		documentID int64
		priority   Priority
	}{
		{documentID: 0, priority: PriorityWebhook},
		{documentID: -1, priority: PriorityWebhook},
		{documentID: 1, priority: 1},
	} {
		if _, err := q.EnqueueCandidate(context.Background(), test.documentID, test.priority); errorCategory(err) != saferr.CategoryValidation {
			t.Errorf("EnqueueCandidate(%d, %d) error = %v, want validation error", test.documentID, test.priority, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.EnqueueCandidate(ctx, 1, PriorityWebhook); err == nil {
		t.Fatal("EnqueueCandidate(canceled context) error = nil, want error")
	}
}

func TestEnqueueCandidateIsAtomicAcrossDatabaseHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q1 := openTestQueue(t, path, testNow)
	q2 := openTestQueue(t, path, testNow)
	const deliveries = 20
	start := make(chan struct{})
	results := make(chan bool, deliveries)
	errs := make(chan error, deliveries)
	var wg sync.WaitGroup
	for i := range deliveries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			q := q1
			if i%2 == 1 {
				q = q2
			}
			created, err := q.EnqueueCandidate(context.Background(), 42, PriorityWebhook)
			results <- created
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	created := 0
	for result := range results {
		if result {
			created++
		}
	}
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent EnqueueCandidate() error = %v", err)
		}
	}
	if created != 1 {
		t.Errorf("created candidates = %d, want 1", created)
	}
	var count int
	if err := q1.db.QueryRow("SELECT count(*) FROM candidates WHERE document_id = 42").Scan(&count); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if count != 1 {
		t.Errorf("candidate count = %d, want 1", count)
	}
}

func TestNextCandidateOrdersByPriorityCreationAndDocumentID(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	if _, err := q.EnqueueCandidate(context.Background(), 30, PriorityBackfill); err != nil {
		t.Fatalf("enqueue backfill candidate: %v", err)
	}
	q.now = func() time.Time { return testNow.Add(time.Second) }
	for _, id := range []int64{20, 10} {
		if _, err := q.EnqueueCandidate(context.Background(), id, PriorityWebhook); err != nil {
			t.Fatalf("enqueue webhook candidate %d: %v", id, err)
		}
	}

	candidate, ok, err := q.NextCandidate(context.Background())
	if err != nil || !ok {
		t.Fatalf("NextCandidate() = (%+v, %t, %v), want candidate", candidate, ok, err)
	}
	if candidate.DocumentID != 10 || candidate.Priority != PriorityWebhook {
		t.Errorf("NextCandidate() = %+v, want webhook document 10", candidate)
	}
}

func TestResolveCandidateAtomicallyEnqueuesAndDeletes(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	if _, err := q.EnqueueCandidate(context.Background(), 42, PriorityWebhook); err != nil {
		t.Fatalf("EnqueueCandidate() error = %v", err)
	}

	job, created, err := q.ResolveCandidate(context.Background(), 42, EnqueueInput{
		DocumentID: 42, SourceChecksum: "checksum", Priority: PriorityWebhook,
		Model: "model", PromptVersion: "v1",
	})
	if err != nil || !created {
		t.Fatalf("ResolveCandidate() = (%+v, %t, %v), want created job", job, created, err)
	}
	if _, ok, err := q.NextCandidate(context.Background()); err != nil || ok {
		t.Fatalf("NextCandidate() after resolve = (_, %t, %v), want empty", ok, err)
	}

	if _, err := q.EnqueueCandidate(context.Background(), 42, PriorityWebhook); err != nil {
		t.Fatalf("reenqueue candidate: %v", err)
	}
	duplicate, created, err := q.ResolveCandidate(context.Background(), 42, EnqueueInput{
		DocumentID: 42, SourceChecksum: "checksum", Priority: PriorityWebhook,
		Model: "other", PromptVersion: "v2",
	})
	if err != nil || created || duplicate.ID != job.ID {
		t.Fatalf("duplicate ResolveCandidate() = (%+v, %t, %v), want suppression", duplicate, created, err)
	}
	if _, ok, err := q.NextCandidate(context.Background()); err != nil || ok {
		t.Fatalf("NextCandidate() after suppressed resolve = (_, %t, %v), want empty", ok, err)
	}
}

func TestResolveCandidateUsesAuthoritativePromotedPriority(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	if _, err := q.EnqueueCandidate(context.Background(), 42, PriorityBackfill); err != nil {
		t.Fatalf("enqueue backfill candidate: %v", err)
	}
	stale, ok, err := q.NextCandidate(context.Background())
	if err != nil || !ok || stale.Priority != PriorityBackfill {
		t.Fatalf("NextCandidate() = (%+v, %t, %v), want backfill", stale, ok, err)
	}
	if _, err := q.EnqueueCandidate(context.Background(), 42, PriorityWebhook); err != nil {
		t.Fatalf("promote candidate: %v", err)
	}

	job, created, err := q.ResolveCandidate(context.Background(), stale.DocumentID, EnqueueInput{
		DocumentID: stale.DocumentID, SourceChecksum: "checksum", Priority: stale.Priority,
		Model: "model", PromptVersion: "v1",
	})
	if err != nil || !created {
		t.Fatalf("ResolveCandidate() = (%+v, %t, %v), want created job", job, created, err)
	}
	if job.Priority != PriorityWebhook {
		t.Errorf("resolved job priority = %d, want %d", job.Priority, PriorityWebhook)
	}
	if _, ok, err := q.NextCandidate(context.Background()); err != nil || ok {
		t.Fatalf("NextCandidate() after resolve = (_, %t, %v), want empty", ok, err)
	}
}

func TestResolveCandidateRetainsCandidateOnInvalidOrMismatchedInput(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	if _, err := q.EnqueueCandidate(context.Background(), 42, PriorityWebhook); err != nil {
		t.Fatalf("EnqueueCandidate() error = %v", err)
	}
	for _, input := range []EnqueueInput{
		{DocumentID: 43, SourceChecksum: "checksum", Priority: PriorityWebhook, Model: "model", PromptVersion: "v1"},
		{DocumentID: 42, SourceChecksum: " ", Priority: PriorityWebhook, Model: "model", PromptVersion: "v1"},
	} {
		if _, _, err := q.ResolveCandidate(context.Background(), 42, input); errorCategory(err) != saferr.CategoryValidation {
			t.Errorf("ResolveCandidate(%+v) error = %v, want validation", input, err)
		}
	}
	if candidate, ok, err := q.NextCandidate(context.Background()); err != nil || !ok || candidate.DocumentID != 42 {
		t.Fatalf("retained NextCandidate() = (%+v, %t, %v), want document 42", candidate, ok, err)
	}
}

func TestResolveCandidateMissingCandidateDoesNotEnqueue(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)

	_, _, err := q.ResolveCandidate(context.Background(), 42, EnqueueInput{
		DocumentID: 42, SourceChecksum: "checksum", Priority: PriorityWebhook,
		Model: "model", PromptVersion: "v1",
	})
	if errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("ResolveCandidate() error = %v, want validation", err)
	}
	var count int
	if err := q.db.QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 0 {
		t.Errorf("job count = %d, want 0", count)
	}
}

func TestDiscardCandidateIsDurableAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q := openTestQueue(t, path, testNow)
	if _, err := q.EnqueueCandidate(context.Background(), 42, PriorityWebhook); err != nil {
		t.Fatal(err)
	}
	if err := q.DiscardCandidate(context.Background(), 42); err != nil {
		t.Fatalf("DiscardCandidate() error = %v", err)
	}
	if err := q.DiscardCandidate(context.Background(), 42); err != nil {
		t.Fatalf("idempotent DiscardCandidate() error = %v", err)
	}
	if err := q.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q = New(db)
	if _, ok, err := q.NextCandidate(context.Background()); err != nil || ok {
		t.Fatalf("NextCandidate() after reopen = (_, %t, %v), want empty", ok, err)
	}
}

func TestDiscardCandidateValidatesDocumentID(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	if err := q.DiscardCandidate(context.Background(), 0); errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("DiscardCandidate(0) error = %v, want validation", err)
	}
}

func TestEnqueueContextCanceledBeforeTransactionCreatesNoJob(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := q.EnqueueContext(ctx, EnqueueInput{
		DocumentID: 1, SourceChecksum: "checksum", Priority: PriorityBackfill,
		Model: "model", PromptVersion: "v1",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnqueueContext() error = %v, want context canceled", err)
	}
	var count int
	if err := q.db.QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("job count = %d, want 0", count)
	}
}

func TestEnqueueContextCancellationWhileWaitingForWriterCreatesNoJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q1 := openTestQueue(t, path, testNow)
	q2 := openTestQueue(t, path, testNow)
	lock := holdWriterLock(t, q1.db)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := q2.EnqueueContext(ctx, EnqueueInput{
			DocumentID: 1, SourceChecksum: "checksum", Priority: PriorityBackfill,
			Model: "model", PromptVersion: "v1",
		})
		result <- err
	}()
	waitForConnectionInUse(t, q2.db)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnqueueContext() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EnqueueContext() did not return after cancellation")
	}
	lock.Release(t)
	var count int
	if err := q1.db.QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("job count = %d, want 0", count)
	}
}

func TestEnqueueIsIdempotentAndPromotesCurrentWork(t *testing.T) {
	now := testNow
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), now)

	first, created, err := q.Enqueue(EnqueueInput{
		DocumentID: 1, SourceChecksum: "checksum", Priority: PriorityBackfill,
		Model: "model-a", PromptVersion: "v1",
	})
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if !created {
		t.Fatal("Enqueue() created = false, want true")
	}

	now = now.Add(time.Second)
	q.now = func() time.Time { return now }
	duplicate, created, err := q.Enqueue(EnqueueInput{
		DocumentID: 1, SourceChecksum: "checksum", Priority: PriorityWebhook,
		Model: "model-b", PromptVersion: "v2",
	})
	if err != nil {
		t.Fatalf("duplicate Enqueue() error = %v", err)
	}
	if created {
		t.Fatal("duplicate Enqueue() created = true, want false")
	}
	if duplicate.ID != first.ID {
		t.Errorf("duplicate job ID = %d, want %d", duplicate.ID, first.ID)
	}
	if duplicate.Priority != PriorityWebhook {
		t.Errorf("duplicate priority = %d, want %d", duplicate.Priority, PriorityWebhook)
	}
	if duplicate.Model != "model-a" || duplicate.PromptVersion != "v1" {
		t.Errorf("duplicate changed processing inputs: model=%q prompt=%q", duplicate.Model, duplicate.PromptVersion)
	}
	persisted := loadJob(t, q, duplicate.ID)
	if !duplicate.UpdatedAt.Equal(now.UTC()) || !duplicate.UpdatedAt.Equal(persisted.UpdatedAt) || !duplicate.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("promoted UpdatedAt = %v, persisted = %v, original = %v, want advanced persisted timestamp",
			duplicate.UpdatedAt, persisted.UpdatedAt, first.UpdatedAt)
	}

	lowered, created, err := q.Enqueue(EnqueueInput{
		DocumentID: 1, SourceChecksum: "checksum", Priority: PriorityBackfill,
		Model: "model-a", PromptVersion: "v1",
	})
	if err != nil {
		t.Fatalf("lower priority Enqueue() error = %v", err)
	}
	if created || lowered.Priority != PriorityWebhook {
		t.Errorf("lower priority duplicate = (%t, %d), want (false, %d)", created, lowered.Priority, PriorityWebhook)
	}
}

func TestEnqueueSuppressesCompletedAndFailedHistory(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	completed := enqueueAndClaim(t, q, 1, "completed", PriorityBackfill, "owner")
	if err := q.Complete(completed.ID, completed.Attempts, "owner"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	job, created, err := q.Enqueue(EnqueueInput{
		DocumentID: 1, SourceChecksum: "completed", Priority: PriorityWebhook,
		Model: "different-model", PromptVersion: "different-prompt",
	})
	if err != nil {
		t.Fatalf("completed Enqueue() error = %v", err)
	}
	if created || job.State != StateCompleted || job.ID != completed.ID {
		t.Errorf("completed Enqueue() = (%+v, %t), want existing completed job", job, created)
	}

	changed, created, err := q.Enqueue(EnqueueInput{
		DocumentID: 1, SourceChecksum: "changed", Priority: PriorityBackfill,
		Model: "model", PromptVersion: "v1",
	})
	if err != nil || !created || changed.ID == completed.ID {
		t.Fatalf("changed checksum Enqueue() = (%+v, %t, %v), want new job", changed, created, err)
	}

	claimed, ok, err := q.Claim("owner", time.Minute)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %t, %v), want job", claimed, ok, err)
	}
	if err := q.Fail(claimed.ID, claimed.Attempts, "owner", SafeDiagnostic{
		Category: saferr.CategoryProvider, Message: "retry limit reached",
	}); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	failed, created, err := q.Enqueue(EnqueueInput{
		DocumentID: 1, SourceChecksum: "changed", Priority: PriorityWebhook,
		Model: "model", PromptVersion: "v2",
	})
	if err != nil || created || failed.State != StateFailed || failed.ID != claimed.ID {
		t.Errorf("failed Enqueue() = (%+v, %t, %v), want existing failed job", failed, created, err)
	}
}

func TestEnqueueValidatesInput(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	tests := []EnqueueInput{
		{DocumentID: 0, SourceChecksum: "checksum", Model: "model", PromptVersion: "v1"},
		{DocumentID: 1, SourceChecksum: " ", Model: "model", PromptVersion: "v1"},
		{DocumentID: 1, SourceChecksum: "checksum", Model: " ", PromptVersion: "v1"},
		{DocumentID: 1, SourceChecksum: "checksum", Model: "model", PromptVersion: " "},
	}
	for i, input := range tests {
		if _, _, err := q.Enqueue(input); errorCategory(err) != saferr.CategoryValidation {
			t.Errorf("case %d Enqueue() error = %v, want validation error", i, err)
		}
	}
}

func TestEnqueueRejectsUndeclaredPriorities(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	for _, priority := range []Priority{-1, 1, 50, PriorityWebhook + 1} {
		input := EnqueueInput{
			DocumentID: 1, SourceChecksum: fmt.Sprintf("checksum-%d", priority),
			Priority: priority, Model: "model", PromptVersion: "v1",
		}
		if _, _, err := q.Enqueue(input); errorCategory(err) != saferr.CategoryValidation {
			t.Errorf("Enqueue(priority %d) error = %v, want validation error", priority, err)
		}
	}

	var count int
	if err := q.db.QueryRow("SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if count != 0 {
		t.Errorf("job count = %d, want 0", count)
	}
}

func TestClaimOrdersByPriorityThenFIFOAndSkipsFutureRetry(t *testing.T) {
	now := testNow
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), now)
	backfill1 := enqueue(t, q, 1, "backfill-1", PriorityBackfill)
	now = now.Add(time.Nanosecond)
	q.now = func() time.Time { return now }
	backfill2 := enqueue(t, q, 2, "backfill-2", PriorityBackfill)
	now = now.Add(time.Nanosecond)
	webhook := enqueue(t, q, 3, "webhook", PriorityWebhook)

	claimed, ok, err := q.Claim("worker", time.Minute)
	if err != nil || !ok || claimed.ID != webhook.ID {
		t.Fatalf("first Claim() = (%+v, %t, %v), want webhook %d", claimed, ok, err, webhook.ID)
	}
	if err := q.Complete(claimed.ID, claimed.Attempts, "worker"); err != nil {
		t.Fatalf("Complete(webhook) error = %v", err)
	}

	claimed, ok, err = q.Claim("worker", time.Minute)
	if err != nil || !ok || claimed.ID != backfill1.ID {
		t.Fatalf("second Claim() = (%+v, %t, %v), want oldest backfill %d", claimed, ok, err, backfill1.ID)
	}
	future := now.Add(time.Hour)
	if err := q.ScheduleRetry(claimed.ID, claimed.Attempts, "worker", future, SafeDiagnostic{
		Category: saferr.CategoryProvider, Message: "temporarily unavailable",
	}); err != nil {
		t.Fatalf("ScheduleRetry() error = %v", err)
	}

	claimed, ok, err = q.Claim("worker", time.Minute)
	if err != nil || !ok || claimed.ID != backfill2.ID {
		t.Fatalf("third Claim() = (%+v, %t, %v), want second backfill %d", claimed, ok, err, backfill2.ID)
	}
	if err := q.Complete(claimed.ID, claimed.Attempts, "worker"); err != nil {
		t.Fatalf("Complete(backfill) error = %v", err)
	}
	if _, ok, err := q.Claim("worker", time.Minute); err != nil || ok {
		t.Fatalf("Claim() before retry due = (_, %t, %v), want no work", ok, err)
	}

	now = future.In(time.FixedZone("later", -5*60*60))
	claimed, ok, err = q.Claim("worker", time.Minute)
	if err != nil || !ok || claimed.ID != backfill1.ID || claimed.Attempts != 2 {
		t.Fatalf("Claim() when retry due = (%+v, %t, %v), want retried job", claimed, ok, err)
	}
}

func TestClaimAllowsOnlyOneActiveJob(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	enqueue(t, q, 1, "one", PriorityBackfill)
	enqueue(t, q, 2, "two", PriorityWebhook)

	first, ok, err := q.Claim("worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first Claim() = (%+v, %t, %v), want job", first, ok, err)
	}
	if _, ok, err := q.Claim("worker-2", time.Minute); err != nil || ok {
		t.Fatalf("second Claim() = (_, %t, %v), want no work", ok, err)
	}
	promoted, created, err := q.Enqueue(EnqueueInput{
		DocumentID: first.DocumentID, SourceChecksum: first.SourceChecksum,
		Priority: PriorityWebhook, Model: "new-model", PromptVersion: "v2",
	})
	if err != nil || created {
		t.Fatalf("processing Enqueue() = (%+v, %t, %v), want existing job", promoted, created, err)
	}
	if promoted.Priority != first.Priority || promoted.LeaseOwner != "worker-1" || promoted.State != StateProcessing {
		t.Errorf("processing job mutated by enqueue = %+v, want unchanged", promoted)
	}
}

func TestClaimIsAtomicAcrossDatabaseHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q1 := openTestQueue(t, path, testNow)
	q2 := openTestQueue(t, path, testNow)
	enqueue(t, q1, 1, "one", PriorityBackfill)
	enqueue(t, q1, 2, "two", PriorityWebhook)

	const claimers = 20
	start := make(chan struct{})
	results := make(chan bool, claimers)
	errs := make(chan error, claimers)
	var wg sync.WaitGroup
	for i := range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			q := q1
			if i%2 == 1 {
				q = q2
			}
			_, ok, err := q.Claim(fmt.Sprintf("worker-%d", i), time.Minute)
			results <- ok
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	claimed := 0
	for ok := range results {
		if ok {
			claimed++
		}
	}
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Claim() error = %v", err)
		}
	}
	if claimed != 1 {
		t.Errorf("successful claims = %d, want 1", claimed)
	}
}

func TestProcessingTransitionsRequireStateAndOwner(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")

	if err := q.Complete(job.ID, job.Attempts, "stale-owner"); errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("stale Complete() error = %v, want validation error", err)
	}
	unchanged := loadJob(t, q, job.ID)
	if unchanged.State != StateProcessing || unchanged.LeaseOwner != "owner" {
		t.Fatalf("job after stale transition = %+v, want unchanged processing job", unchanged)
	}

	if err := q.Complete(job.ID, job.Attempts, "owner"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	completed := loadJob(t, q, job.ID)
	if completed.State != StateCompleted || completed.CompletedAt.IsZero() || completed.LeaseOwner != "" || completed.ErrorCategory != "" {
		t.Errorf("completed job = %+v", completed)
	}
	if err := q.Fail(job.ID, job.Attempts, "owner", SafeDiagnostic{
		Category: saferr.CategoryInternal, Message: "late failure",
	}); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("illegal Fail() error = %v, want validation error", err)
	}
}

func TestExpiredClaimRejectsAllProcessingTransitions(t *testing.T) {
	now := testNow
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), now)
	job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner-a")
	now = job.LeaseExpiresAt
	q.now = func() time.Time { return now }
	diagnostic := SafeDiagnostic{Category: saferr.CategoryProvider, Message: "safe failure"}

	tests := []struct {
		name       string
		transition func() error
	}{
		{"complete", func() error { return q.Complete(job.ID, job.Attempts, "owner-a") }},
		{"fail", func() error { return q.Fail(job.ID, job.Attempts, "owner-a", diagnostic) }},
		{"retry", func() error {
			return q.ScheduleRetry(job.ID, job.Attempts, "owner-a", now.Add(time.Minute), diagnostic)
		}},
	}
	for _, test := range tests {
		before := loadJob(t, q, job.ID)
		if err := test.transition(); errorCategory(err) != saferr.CategoryValidation {
			t.Errorf("%s expired transition error = %v, want validation error", test.name, err)
		}
		if after := loadJob(t, q, job.ID); after != before {
			t.Errorf("job after expired %s = %+v, want unchanged %+v", test.name, after, before)
		}
	}
}

func TestAttemptFencesReclaimedLeaseWithSameOwner(t *testing.T) {
	now := testNow
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), now)
	first := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner-a")
	now = first.LeaseExpiresAt
	q.now = func() time.Time { return now }
	if recovered, err := q.RecoverExpiredLeases(); err != nil || recovered != 1 {
		t.Fatalf("RecoverExpiredLeases() = (%d, %v), want (1, nil)", recovered, err)
	}
	second, ok, err := q.Claim("owner-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("reclaim Claim() = (%+v, %t, %v), want job", second, ok, err)
	}
	if second.Attempts != first.Attempts+1 {
		t.Fatalf("reclaimed attempts = %d, want %d", second.Attempts, first.Attempts+1)
	}

	if err := q.Complete(first.ID, first.Attempts, "owner-a"); errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("late Complete() error = %v, want validation error", err)
	}
	assertProcessingClaim(t, q, second.ID, second.Attempts, "owner-a")
	if err := q.Complete(second.ID, second.Attempts, "owner-a"); err != nil {
		t.Fatalf("current Complete() error = %v", err)
	}
	if got := loadJob(t, q, second.ID); got.State != StateCompleted {
		t.Errorf("current job state = %q, want completed", got.State)
	}
}

func TestCompleteValidatesLeaseAfterWaitingForWriterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q1 := openTestQueue(t, path, testNow)
	q2 := openTestQueue(t, path, testNow)
	clock := newTestClock(testNow)
	q2.now = clock.Now
	job := enqueueAndClaim(t, q1, 1, "checksum", PriorityBackfill, "owner")
	before := loadJob(t, q1, job.ID)

	lock := holdWriterLock(t, q1.db)
	result := make(chan error, 1)
	go func() {
		result <- q2.Complete(job.ID, job.Attempts, "owner")
	}()
	waitForConnectionInUse(t, q2.db)
	clock.Set(job.LeaseExpiresAt)
	lock.Release(t)

	if err := <-result; errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("delayed Complete() error = %v, want validation error", err)
	}
	if after := loadJob(t, q1, job.ID); after != before {
		t.Errorf("job after delayed Complete() = %+v, want unchanged %+v", after, before)
	}
}

func TestScheduleRetryValidatesAvailabilityAfterWaitingForWriterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q1 := openTestQueue(t, path, testNow)
	q2 := openTestQueue(t, path, testNow)
	clock := newTestClock(testNow)
	q2.now = clock.Now
	job := enqueueAndClaim(t, q1, 1, "checksum", PriorityBackfill, "owner")
	before := loadJob(t, q1, job.ID)
	availableAt := testNow.Add(time.Minute)
	diagnostic := SafeDiagnostic{Category: saferr.CategoryProvider, Message: "safe retry"}

	lock := holdWriterLock(t, q1.db)
	result := make(chan error, 1)
	go func() {
		result <- q2.ScheduleRetry(job.ID, job.Attempts, "owner", availableAt, diagnostic)
	}()
	waitForConnectionInUse(t, q2.db)
	clock.Set(availableAt)
	lock.Release(t)

	if err := <-result; errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("delayed ScheduleRetry() error = %v, want validation error", err)
	}
	if after := loadJob(t, q1, job.ID); after != before {
		t.Errorf("job after delayed ScheduleRetry() = %+v, want unchanged %+v", after, before)
	}
}

func TestProcessingTransitionsRejectInvalidAttemptWithoutMutation(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
	diagnostic := SafeDiagnostic{Category: saferr.CategoryProvider, Message: "safe failure"}
	for _, attempt := range []int{-1, 0, job.Attempts + 1} {
		transitions := []struct {
			name       string
			transition func() error
		}{
			{"complete", func() error { return q.Complete(job.ID, attempt, "owner") }},
			{"fail", func() error { return q.Fail(job.ID, attempt, "owner", diagnostic) }},
			{"retry", func() error {
				return q.ScheduleRetry(job.ID, attempt, "owner", testNow.Add(time.Minute), diagnostic)
			}},
		}
		for _, transition := range transitions {
			before := loadJob(t, q, job.ID)
			if err := transition.transition(); errorCategory(err) != saferr.CategoryValidation {
				t.Errorf("%s(attempt %d) error = %v, want validation error", transition.name, attempt, err)
			}
			if after := loadJob(t, q, job.ID); after != before {
				t.Errorf("job after %s attempt %d = %+v, want unchanged %+v", transition.name, attempt, after, before)
			}
		}
	}
}

func TestFailureAndExplicitRetry(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
	if err := q.Fail(job.ID, job.Attempts, "owner", SafeDiagnostic{
		Category: saferr.CategoryProvider, Message: "retry limit reached",
	}); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	failed := loadJob(t, q, job.ID)
	if failed.State != StateFailed || failed.ErrorCategory != saferr.CategoryProvider || failed.ErrorMessage != "retry limit reached" || failed.CompletedAt.IsZero() {
		t.Errorf("failed job = %+v", failed)
	}

	availableAt := testNow.Add(time.Hour)
	if err := q.RetryFailed(job.ID, availableAt); err != nil {
		t.Fatalf("RetryFailed() error = %v", err)
	}
	retry := loadJob(t, q, job.ID)
	if retry.State != StateRetry || !retry.AvailableAt.Equal(availableAt.UTC()) || !retry.CompletedAt.IsZero() || retry.ErrorCategory != "" {
		t.Errorf("retried job = %+v", retry)
	}
	if err := q.RetryFailed(job.ID, availableAt); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("illegal RetryFailed() error = %v, want validation error", err)
	}
}

func TestRetryFailedRejectsConflictingCurrentJob(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	failed := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
	if err := q.Fail(failed.ID, failed.Attempts, "owner", SafeDiagnostic{
		Category: saferr.CategoryProvider, Message: "retry limit reached",
	}); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	now := formatTime(testNow)
	result, err := q.db.Exec(`INSERT INTO jobs (
		document_id, source_checksum, priority, state, attempts, available_at,
		model, prompt_version, created_at, updated_at
	) VALUES (1, 'checksum', ?, 'pending', 0, ?, 'model', 'v1', ?, ?)`,
		PriorityWebhook, now, now, now)
	if err != nil {
		t.Fatalf("insert conflicting current job: %v", err)
	}
	currentID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("current job ID: %v", err)
	}

	err = q.RetryFailed(failed.ID, testNow.Add(time.Minute))
	if errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("RetryFailed() error = %v, want validation error", err)
	}
	if got := loadJob(t, q, failed.ID); got.State != StateFailed {
		t.Errorf("failed history state = %q, want failed", got.State)
	}
	if got := loadJob(t, q, currentID); got.State != StatePending {
		t.Errorf("current job state = %q, want pending", got.State)
	}
}

func TestRetryValidation(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
	valid := SafeDiagnostic{Category: saferr.CategoryProvider, Message: "retry"}
	if err := q.ScheduleRetry(job.ID, job.Attempts, "owner", testNow, valid); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("ScheduleRetry(now) error = %v, want validation error", err)
	}
	if _, _, err := q.Claim("", time.Minute); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("Claim(blank owner) error = %v, want validation error", err)
	}
	if err := q.Complete(job.ID, job.Attempts, ""); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("Complete(blank owner) error = %v, want validation error", err)
	}
}

func TestTransitionDiagnosticsValidateAndPersistSafeValues(t *testing.T) {
	validCategories := []saferr.Category{
		saferr.CategoryConfiguration,
		saferr.CategoryPaperless,
		saferr.CategoryProvider,
		saferr.CategoryValidation,
		saferr.CategoryRendering,
		saferr.CategoryInternal,
	}
	for i, category := range validCategories {
		q := openTestQueue(t, filepath.Join(t.TempDir(), fmt.Sprintf("valid-%d.db", i)), testNow)
		job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
		diagnostic := SafeDiagnostic{Category: category, Message: "Caller-vetted safe value 123"}
		if err := q.Fail(job.ID, job.Attempts, "owner", diagnostic); err != nil {
			t.Fatalf("Fail(%q) error = %v", category, err)
		}
		failed := loadJob(t, q, job.ID)
		if failed.ErrorCategory != category || failed.ErrorMessage != diagnostic.Message {
			t.Errorf("persisted diagnostic = (%q, %q), want (%q, %q)",
				failed.ErrorCategory, failed.ErrorMessage, category, diagnostic.Message)
		}
	}

	invalid := []SafeDiagnostic{
		{Category: saferr.Category("unknown"), Message: "safe message"},
		{Category: saferr.CategoryProvider, Message: ""},
		{Category: saferr.CategoryProvider, Message: "   "},
		{Category: saferr.CategoryProvider, Message: strings.Repeat("x", maxDiagnosticMessageBytes+1)},
		{Category: saferr.CategoryProvider, Message: "two\nlines"},
		{Category: saferr.CategoryProvider, Message: "tab\tseparated"},
		{Category: saferr.CategoryProvider, Message: "escape\x1bcode"},
		{Category: saferr.CategoryProvider, Message: "delete\x7fcode"},
	}
	for i, diagnostic := range invalid {
		q := openTestQueue(t, filepath.Join(t.TempDir(), fmt.Sprintf("invalid-%d.db", i)), testNow)
		job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
		if err := q.Fail(job.ID, job.Attempts, "owner", diagnostic); errorCategory(err) != saferr.CategoryValidation {
			t.Errorf("Fail(%+v) error = %v, want validation error", diagnostic, err)
		}
		if got := loadJob(t, q, job.ID); got.State != StateProcessing || got.ErrorCategory != "" {
			t.Errorf("job after invalid diagnostic = %+v, want unchanged processing job", got)
		}
	}
}

func TestScheduleRetryPersistsSafeDiagnosticUnchanged(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
	diagnostic := SafeDiagnostic{
		Category: saferr.CategoryRendering,
		Message:  strings.Repeat("x", maxDiagnosticMessageBytes),
	}
	if err := q.ScheduleRetry(job.ID, job.Attempts, "owner", testNow.Add(time.Minute), diagnostic); err != nil {
		t.Fatalf("ScheduleRetry() error = %v", err)
	}
	retry := loadJob(t, q, job.ID)
	if retry.ErrorCategory != diagnostic.Category || retry.ErrorMessage != diagnostic.Message {
		t.Errorf("persisted diagnostic = (%q, %q), want (%q, %q)",
			retry.ErrorCategory, retry.ErrorMessage, diagnostic.Category, diagnostic.Message)
	}
}

func TestRecoverExpiredLeases(t *testing.T) {
	now := testNow
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), now)
	expired := enqueueAndClaim(t, q, 1, "expired", PriorityBackfill, "old-owner")
	now = now.Add(2 * time.Minute)
	q.now = func() time.Time { return now }

	recovered, err := q.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases() error = %v", err)
	}
	if recovered != 1 {
		t.Errorf("RecoverExpiredLeases() = %d, want 1", recovered)
	}
	retry := loadJob(t, q, expired.ID)
	if retry.State != StateRetry || retry.LeaseOwner != "" || retry.ErrorCategory != saferr.CategoryInternal || retry.ErrorMessage != "lease expired" || !retry.AvailableAt.Equal(now.UTC()) {
		t.Errorf("recovered job = %+v", retry)
	}
	if err := q.Complete(expired.ID, expired.Attempts, "old-owner"); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("stale Complete() error = %v, want validation error", err)
	}

	claimed, ok, err := q.Claim("new-owner", time.Minute)
	if err != nil || !ok || claimed.ID != expired.ID {
		t.Fatalf("Claim(recovered) = (%+v, %t, %v), want recovered job", claimed, ok, err)
	}
	if recovered, err := q.RecoverExpiredLeases(); err != nil || recovered != 0 {
		t.Errorf("RecoverExpiredLeases(unexpired) = (%d, %v), want (0, nil)", recovered, err)
	}
}

func TestRecoverExpiredLeasesIncludesExactBoundaryOnly(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	exact := enqueue(t, q, 1, "exact", PriorityBackfill)
	future := enqueue(t, q, 2, "future", PriorityBackfill)
	now := formatTime(testNow)
	if _, err := q.db.Exec(`UPDATE jobs SET
		state = 'processing', attempts = 1, lease_owner = 'exact-owner',
		lease_expires_at = ?, updated_at = ? WHERE id = ?`, now, now, exact.ID); err != nil {
		t.Fatalf("set exact lease: %v", err)
	}
	if _, err := q.db.Exec(`UPDATE jobs SET
		state = 'processing', attempts = 1, lease_owner = 'future-owner',
		lease_expires_at = ?, updated_at = ? WHERE id = ?`,
		formatTime(testNow.Add(time.Nanosecond)), now, future.ID); err != nil {
		t.Fatalf("set future lease: %v", err)
	}

	recovered, err := q.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases() error = %v", err)
	}
	if recovered != 1 {
		t.Errorf("RecoverExpiredLeases() = %d, want 1", recovered)
	}
	if got := loadJob(t, q, exact.ID); got.State != StateRetry {
		t.Errorf("exact-boundary job state = %q, want retry", got.State)
	}
	if got := loadJob(t, q, future.ID); got.State != StateProcessing || got.LeaseOwner != "future-owner" {
		t.Errorf("future job = %+v, want unchanged processing lease", got)
	}
}

func TestPublicDatabaseErrorsDoNotExposeDriverDetails(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	if _, err := q.db.Exec("DROP TABLE jobs"); err != nil {
		t.Fatalf("drop jobs table: %v", err)
	}

	_, _, err := q.Enqueue(EnqueueInput{
		DocumentID: 1, SourceChecksum: "sensitive-checksum", Priority: PriorityBackfill,
		Model: "sensitive-model", PromptVersion: "sensitive-prompt",
	})
	if errorCategory(err) != saferr.CategoryInternal {
		t.Fatalf("Enqueue() error = %v, want internal error", err)
	}
	for _, forbidden := range []string{"no such table", "sensitive-checksum", "sensitive-model", "sensitive-prompt"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("Enqueue() error %q exposes %q", err, forbidden)
		}
	}
}

func openTestQueue(t *testing.T, path string, now time.Time) *Queue {
	t.Helper()
	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	q := New(db)
	q.now = func() time.Time { return now }
	return q
}

func enqueue(t *testing.T, q *Queue, documentID int64, checksum string, priority Priority) Job {
	t.Helper()
	job, created, err := q.Enqueue(EnqueueInput{
		DocumentID: documentID, SourceChecksum: checksum, Priority: priority,
		Model: "model", PromptVersion: "v1",
	})
	if err != nil || !created {
		t.Fatalf("Enqueue() = (%+v, %t, %v), want created job", job, created, err)
	}
	return job
}

func enqueueAndClaim(t *testing.T, q *Queue, documentID int64, checksum string, priority Priority, owner string) Job {
	t.Helper()
	enqueue(t, q, documentID, checksum, priority)
	job, ok, err := q.Claim(owner, time.Minute)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %t, %v), want job", job, ok, err)
	}
	return job
}

func loadJob(t *testing.T, q *Queue, id int64) Job {
	t.Helper()
	job, err := q.get(id)
	if err != nil {
		t.Fatalf("get(%d) error = %v", id, err)
	}
	return job
}

func assertProcessingClaim(t *testing.T, q *Queue, id int64, attempt int, owner string) {
	t.Helper()
	job := loadJob(t, q, id)
	if job.State != StateProcessing || job.Attempts != attempt || job.LeaseOwner != owner {
		t.Fatalf("job = %+v, want processing attempt %d owner %q", job, attempt, owner)
	}
}

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func newTestClock(now time.Time) *testClock {
	return &testClock{now: now}
}

func (clock *testClock) Now() time.Time {
	clock.mu.RLock()
	defer clock.mu.RUnlock()
	return clock.now
}

func (clock *testClock) Set(now time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = now
}

type writerLock struct {
	conn *sql.Conn
}

func holdWriterLock(t *testing.T, db *sql.DB) writerLock {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire writer connection: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		t.Fatalf("begin writer transaction: %v", err)
	}
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), "ROLLBACK")
		conn.Close()
	})
	return writerLock{conn: conn}
}

func (lock writerLock) Release(t *testing.T) {
	t.Helper()
	if _, err := lock.conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release writer transaction: %v", err)
	}
	if err := lock.conn.Close(); err != nil {
		t.Fatalf("close writer connection: %v", err)
	}
}

func waitForConnectionInUse(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for db.Stats().InUse != 1 {
		if time.Now().After(deadline) {
			t.Fatal("transition did not reach database contention")
		}
		time.Sleep(time.Millisecond)
	}
}

func errorCategory(err error) saferr.Category {
	var safeError *saferr.Error
	if errors.As(err, &safeError) {
		return safeError.Category()
	}
	return ""
}
