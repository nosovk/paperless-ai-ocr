package queue

import (
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

func TestEnqueueIsIdempotentAndPromotesCurrentWork(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)

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
	if err := q.Complete(completed.ID, "owner"); err != nil {
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
	if err := q.Fail(claimed.ID, "owner", "provider", "retry limit reached"); err != nil {
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
	if err := q.Complete(claimed.ID, "worker"); err != nil {
		t.Fatalf("Complete(webhook) error = %v", err)
	}

	claimed, ok, err = q.Claim("worker", time.Minute)
	if err != nil || !ok || claimed.ID != backfill1.ID {
		t.Fatalf("second Claim() = (%+v, %t, %v), want oldest backfill %d", claimed, ok, err, backfill1.ID)
	}
	future := now.Add(time.Hour)
	if err := q.ScheduleRetry(claimed.ID, "worker", future, "provider", "temporarily unavailable"); err != nil {
		t.Fatalf("ScheduleRetry() error = %v", err)
	}

	claimed, ok, err = q.Claim("worker", time.Minute)
	if err != nil || !ok || claimed.ID != backfill2.ID {
		t.Fatalf("third Claim() = (%+v, %t, %v), want second backfill %d", claimed, ok, err, backfill2.ID)
	}
	if err := q.Complete(claimed.ID, "worker"); err != nil {
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

	if err := q.Complete(job.ID, "stale-owner"); errorCategory(err) != saferr.CategoryValidation {
		t.Fatalf("stale Complete() error = %v, want validation error", err)
	}
	unchanged := loadJob(t, q, job.ID)
	if unchanged.State != StateProcessing || unchanged.LeaseOwner != "owner" {
		t.Fatalf("job after stale transition = %+v, want unchanged processing job", unchanged)
	}

	if err := q.Complete(job.ID, "owner"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	completed := loadJob(t, q, job.ID)
	if completed.State != StateCompleted || completed.CompletedAt.IsZero() || completed.LeaseOwner != "" || completed.ErrorCategory != "" {
		t.Errorf("completed job = %+v", completed)
	}
	if err := q.Fail(job.ID, "owner", "internal", "late failure"); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("illegal Fail() error = %v, want validation error", err)
	}
}

func TestFailureAndExplicitRetry(t *testing.T) {
	q := openTestQueue(t, filepath.Join(t.TempDir(), "queue.db"), testNow)
	job := enqueueAndClaim(t, q, 1, "checksum", PriorityBackfill, "owner")
	if err := q.Fail(job.ID, "owner", "provider", "retry limit reached"); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	failed := loadJob(t, q, job.ID)
	if failed.State != StateFailed || failed.ErrorCategory != "provider" || failed.ErrorMessage != "retry limit reached" || failed.CompletedAt.IsZero() {
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
	if err := q.Fail(failed.ID, "owner", "provider", "retry limit reached"); err != nil {
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
	if err := q.ScheduleRetry(job.ID, "owner", testNow, "provider", "retry"); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("ScheduleRetry(now) error = %v, want validation error", err)
	}
	if err := q.ScheduleRetry(job.ID, "owner", testNow.Add(time.Minute), "", "retry"); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("ScheduleRetry(blank category) error = %v, want validation error", err)
	}
	if _, _, err := q.Claim("", time.Minute); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("Claim(blank owner) error = %v, want validation error", err)
	}
	if err := q.Complete(job.ID, ""); errorCategory(err) != saferr.CategoryValidation {
		t.Errorf("Complete(blank owner) error = %v, want validation error", err)
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
	if retry.State != StateRetry || retry.LeaseOwner != "" || retry.ErrorCategory != "internal" || retry.ErrorMessage != "lease expired" || !retry.AvailableAt.Equal(now.UTC()) {
		t.Errorf("recovered job = %+v", retry)
	}
	if err := q.Complete(expired.ID, "old-owner"); errorCategory(err) != saferr.CategoryValidation {
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

func errorCategory(err error) saferr.Category {
	var safeError *saferr.Error
	if errors.As(err, &safeError) {
		return safeError.Category()
	}
	return ""
}
