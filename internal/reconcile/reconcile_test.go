package reconcile

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/database"
	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

func TestRunOnceResolvesWebhookCandidatesBeforeBoundedBackfill(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	for _, id := range []int64{2, 1} {
		if _, err := q.EnqueueCandidate(context.Background(), id, queue.PriorityWebhook); err != nil {
			t.Fatalf("enqueue candidate %d: %v", id, err)
		}
	}
	if _, err := db.Exec("UPDATE candidates SET created_at = '2026-08-29T12:00:00.000000000Z'"); err != nil {
		t.Fatalf("equalize candidate timestamps: %v", err)
	}
	client, requests := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/documents/1/":
			io.WriteString(writer, `{"id":1,"checksum":"webhook-1","tags":[91]}`)
		case "/api/documents/2/":
			io.WriteString(writer, `{"id":2,"checksum":"webhook-2","tags":[]}`)
		case "/api/documents/":
			io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":3,"checksum":"backfill","tags":[92]}]}`)
		default:
			http.NotFound(writer, request)
		}
	})
	r := newReconciler(t, db, client, q, Options{MaxCandidatesPerPass: 10, MaxArchivePagesPerPass: 1})

	report, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if report.CandidatesResolved != 2 || report.PagesProcessed != 1 || report.JobsCreated != 3 || !report.ScanComplete {
		t.Errorf("RunOnce() report = %+v", report)
	}
	if got := requests(); !slices.Equal(got, []string{"/api/documents/1/", "/api/documents/2/", "/api/documents/"}) {
		t.Errorf("request order = %v", got)
	}
	assertClaimOrder(t, q, []int64{1, 2, 3})
}

func TestRunOnceCandidateLimitYieldsBeforeBackfill(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	for _, id := range []int64{1, 2} {
		if _, err := q.EnqueueCandidate(context.Background(), id, queue.PriorityWebhook); err != nil {
			t.Fatal(err)
		}
	}
	client, requests := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		var id int
		if _, err := fmt.Sscanf(request.URL.Path, "/api/documents/%d/", &id); err != nil {
			t.Fatalf("parse document request %q: %v", request.URL.Path, err)
		}
		fmt.Fprintf(writer, `{"id":%d,"checksum":"candidate-%d","tags":[]}`, id, id)
	})
	r := newReconciler(t, db, client, q, Options{MaxCandidatesPerPass: 1, MaxArchivePagesPerPass: 1})

	report, err := r.RunOnce(context.Background())
	if err != nil || report.CandidatesResolved != 1 || report.PagesProcessed != 0 {
		t.Fatalf("RunOnce() = (%+v, %v), want one candidate only", report, err)
	}
	if got := requests(); len(got) != 1 || got[0] != "/api/documents/1/" {
		t.Errorf("requests = %v, want first deterministic candidate", got)
	}
}

func TestRunOnceTemporaryCandidateFailureRetainsCandidateAndStops(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	if _, err := q.EnqueueCandidate(context.Background(), 1, queue.PriorityWebhook); err != nil {
		t.Fatal(err)
	}
	client, requests := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "sensitive body", http.StatusInternalServerError)
	})
	r := newReconciler(t, db, client, q, Options{})

	if _, err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want temporary Paperless error")
	}
	if candidate, ok, err := q.NextCandidate(context.Background()); err != nil || !ok || candidate.DocumentID != 1 {
		t.Fatalf("candidate after failure = (%+v, %t, %v)", candidate, ok, err)
	}
	if got := requests(); !slices.Equal(got, []string{"/api/documents/1/"}) {
		t.Errorf("requests = %v, want no archive request", got)
	}
}

func TestRunOnceDiscardsMissingCandidateThenContinuesToBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconcile.db")
	db, q := openStore(t, path)
	for _, id := range []int64{1, 2} {
		if _, err := q.EnqueueCandidate(context.Background(), id, queue.PriorityWebhook); err != nil {
			t.Fatal(err)
		}
	}
	client, requests := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/documents/1/":
			http.NotFound(writer, request)
		case "/api/documents/2/":
			io.WriteString(writer, `{"id":2,"checksum":"candidate","tags":[]}`)
		case "/api/documents/":
			io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":3,"checksum":"backfill","tags":[]}]}`)
		}
	})
	r := newReconciler(t, db, client, q, Options{})

	report, err := r.RunOnce(context.Background())
	if err != nil || report.CandidatesDiscarded != 1 || report.CandidatesResolved != 1 || report.JobsCreated != 2 || !report.ScanComplete {
		t.Fatalf("RunOnce() = (%+v, %v)", report, err)
	}
	if got := requests(); !slices.Equal(got, []string{"/api/documents/1/", "/api/documents/2/", "/api/documents/"}) {
		t.Errorf("requests = %v", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q = queue.New(db)
	if _, ok, err := q.NextCandidate(context.Background()); err != nil || ok {
		t.Fatalf("candidate after reopen = (_, %t, %v), want empty", ok, err)
	}
	assertClaimOrder(t, q, []int64{2, 3})
}

func TestRunOnceDiscardsBlankChecksumCandidateThenContinues(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	for _, id := range []int64{1, 2} {
		if _, err := q.EnqueueCandidate(context.Background(), id, queue.PriorityWebhook); err != nil {
			t.Fatal(err)
		}
	}
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/documents/1/":
			io.WriteString(writer, `{"id":1,"checksum":" ","content":"sensitive","tags":[]}`)
		case "/api/documents/2/":
			io.WriteString(writer, `{"id":2,"checksum":"valid","tags":[]}`)
		case "/api/documents/":
			io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
		}
	})
	r := newReconciler(t, db, client, q, Options{})

	report, err := r.RunOnce(context.Background())
	if err != nil || report.CandidatesDiscarded != 1 || report.CandidatesResolved != 1 || !report.ScanComplete {
		t.Fatalf("RunOnce() = (%+v, %v)", report, err)
	}
	job, ok, err := q.Claim("worker", time.Minute)
	if err != nil || !ok || job.DocumentID != 2 {
		t.Fatalf("Claim() = (%+v, %t, %v), want valid candidate", job, ok, err)
	}
}

func TestRunOnceUsesCandidatePriorityPromotedDuringPaperlessRequest(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	if _, err := q.EnqueueCandidate(context.Background(), 1, queue.PriorityBackfill); err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/documents/1/" {
			close(requestStarted)
			<-releaseRequest
			io.WriteString(writer, `{"id":1,"checksum":"checksum","tags":[]}`)
			return
		}
		io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
	})
	r := newReconciler(t, db, client, q, Options{})
	type result struct {
		report Report
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		report, err := r.RunOnce(context.Background())
		resultCh <- result{report: report, err: err}
	}()

	<-requestStarted
	if _, err := q.EnqueueCandidate(context.Background(), 1, queue.PriorityWebhook); err != nil {
		t.Fatalf("promote candidate during request: %v", err)
	}
	close(releaseRequest)
	resultValue := <-resultCh
	if resultValue.err != nil || resultValue.report.CandidatesResolved != 1 {
		t.Fatalf("RunOnce() = (%+v, %v), want resolved candidate", resultValue.report, resultValue.err)
	}
	if _, ok, err := q.NextCandidate(context.Background()); err != nil || ok {
		t.Fatalf("NextCandidate() after RunOnce = (_, %t, %v), want empty", ok, err)
	}
	job, ok, err := q.Claim("worker", time.Minute)
	if err != nil || !ok || job.Priority != queue.PriorityWebhook {
		t.Fatalf("Claim() = (%+v, %t, %v), want webhook priority", job, ok, err)
	}
}

func TestRunOncePreservesPaperlessErrorCategoryAndCause(t *testing.T) {
	for _, test := range []struct {
		name      string
		candidate bool
	}{
		{name: "candidate", candidate: true},
		{name: "archive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
			if test.candidate {
				if _, err := q.EnqueueCandidate(context.Background(), 1, queue.PriorityWebhook); err != nil {
					t.Fatal(err)
				}
			}
			client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
				http.Error(writer, "sensitive response body", http.StatusInternalServerError)
			})
			r := newReconciler(t, db, client, q, Options{})

			_, err := r.RunOnce(context.Background())
			if errorCategory(err) != saferr.CategoryPaperless {
				t.Fatalf("RunOnce() error = %v, category = %q, want paperless", err, errorCategory(err))
			}
			var statusErr *paperless.StatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusInternalServerError {
				t.Errorf("RunOnce() error cause = %v, want Paperless status 500", err)
			}
			if strings.Contains(err.Error(), "sensitive response body") {
				t.Errorf("RunOnce() error %q exposes response body", err)
			}
		})
	}
}

func TestRunOncePreservesInternalQueueErrorCategory(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	if _, err := db.Exec("DROP TABLE candidates"); err != nil {
		t.Fatalf("drop candidates: %v", err)
	}
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("Paperless request made after queue failure")
	})
	r := newReconciler(t, db, client, q, Options{})

	_, err := r.RunOnce(context.Background())
	if errorCategory(err) != saferr.CategoryInternal {
		t.Fatalf("RunOnce() error = %v, category = %q, want internal", err, errorCategory(err))
	}
	if strings.Contains(err.Error(), "no such table") {
		t.Errorf("RunOnce() error %q exposes database details", err)
	}
}

func TestRunOncePersistsBoundedArchiveCheckpointAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconcile.db")
	db, q := openStore(t, path)
	client, requests := paginatedPaperless(t, 3, 0)
	r := newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 1})

	first, err := r.RunOnce(context.Background())
	if err != nil || first.PagesProcessed != 1 || first.ScanComplete {
		t.Fatalf("first RunOnce() = (%+v, %v)", first, err)
	}
	checkpoint := loadCheckpointValue(t, db)
	if checkpoint["version"] != float64(checkpointVersion) || checkpoint["pages"] != float64(1) {
		t.Errorf("checkpoint metadata = %#v", checkpoint)
	}
	visited, ok := checkpoint["visited"].([]any)
	if !ok || len(visited) != 1 || visited[0] != checkpointFingerprint(initialCursorIdentifier) {
		t.Errorf("checkpoint visited = %#v, want initial cursor fingerprint", checkpoint["visited"])
	}
	if strings.Contains(fmt.Sprint(checkpoint["visited"]), "api/documents") {
		t.Errorf("checkpoint visited contains raw cursor: %#v", checkpoint["visited"])
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	db, err = database.Open(path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	q = queue.New(db)
	r = newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 1})
	second, err := r.RunOnce(context.Background())
	if err != nil || second.PagesProcessed != 1 || second.ScanComplete {
		t.Fatalf("second RunOnce() = (%+v, %v)", second, err)
	}
	third, err := r.RunOnce(context.Background())
	if err != nil || !third.ScanComplete {
		t.Fatalf("third RunOnce() = (%+v, %v)", third, err)
	}
	fourth, err := r.RunOnce(context.Background())
	if err != nil || fourth.DocumentsSeen != 1 {
		t.Fatalf("fourth RunOnce() = (%+v, %v), want new scan from page one", fourth, err)
	}
	if got := requests(); !slices.Equal(got, []string{"1", "2", "3", "1"}) {
		t.Errorf("requested pages = %v", got)
	}
}

func TestRunOncePageFailureKeepsPriorProgressAndResumes(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	client, requests := paginatedPaperless(t, 3, 2)
	r := newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 3})

	report, err := r.RunOnce(context.Background())
	if err == nil || report.PagesProcessed != 1 || report.JobsCreated != 1 {
		t.Fatalf("failing RunOnce() = (%+v, %v)", report, err)
	}
	report, err = r.RunOnce(context.Background())
	if err != nil || report.PagesProcessed != 2 || !report.ScanComplete {
		t.Fatalf("resumed RunOnce() = (%+v, %v)", report, err)
	}
	if got := requests(); !slices.Equal(got, []string{"1", "2", "2", "3"}) {
		t.Errorf("requested pages = %v", got)
	}
}

func TestRunOnceUsesSQLiteStateNotTagsAndHandlesChecksumChanges(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	old, _, err := q.Enqueue(queue.EnqueueInput{DocumentID: 1, SourceChecksum: "old", Priority: queue.PriorityBackfill, Model: "old-model", PromptVersion: "old-prompt"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := q.Claim("worker", time.Minute)
	if err != nil || !ok || claimed.ID != old.ID {
		t.Fatalf("claim old job: %+v %t %v", claimed, ok, err)
	}
	if err := q.Complete(claimed.ID, claimed.Attempts, "worker"); err != nil {
		t.Fatal(err)
	}
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, `{"count":2,"next":null,"results":[{"id":1,"checksum":"old","tags":[999]},{"id":2,"checksum":"new","tags":[999]}]}`)
	})
	r := newReconciler(t, db, client, q, Options{Model: "new-model", PromptVersion: "new-prompt"})

	report, err := r.RunOnce(context.Background())
	if err != nil || report.JobsCreated != 1 {
		t.Fatalf("suppression RunOnce() = (%+v, %v)", report, err)
	}
	job, ok, err := q.Claim("worker", time.Minute)
	if err != nil || !ok || job.DocumentID != 2 || job.Priority != queue.PriorityBackfill {
		t.Fatalf("Claim() = (%+v, %t, %v), want misleading-tag document", job, ok, err)
	}
	if err := q.Complete(job.ID, job.Attempts, "worker"); err != nil {
		t.Fatal(err)
	}

	client, _ = testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":1,"checksum":"changed","tags":[999]}]}`)
	})
	r = newReconciler(t, db, client, q, Options{})
	report, err = r.RunOnce(context.Background())
	if err != nil || report.JobsCreated != 1 {
		t.Fatalf("checksum-change RunOnce() = (%+v, %v)", report, err)
	}
}

func TestRunOnceDoesNotDemoteWebhookJobSeenInArchive(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	if _, err := q.EnqueueCandidate(context.Background(), 1, queue.PriorityWebhook); err != nil {
		t.Fatal(err)
	}
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/documents/1/" {
			io.WriteString(writer, `{"id":1,"checksum":"same","tags":[]}`)
			return
		}
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":1,"checksum":"same","tags":[]}]}`)
	})
	r := newReconciler(t, db, client, q, Options{})
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, ok, err := q.Claim("worker", time.Minute)
	if err != nil || !ok || job.Priority != queue.PriorityWebhook {
		t.Fatalf("Claim() = (%+v, %t, %v), want webhook priority", job, ok, err)
	}
}

func TestRunOnceYieldErrorPreservesCompletedPageCheckpoint(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	client, requests := paginatedPaperless(t, 2, 0)
	yieldErr := errors.New("stop yielding")
	r := newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 2, Yield: func(context.Context) error { return yieldErr }})

	report, err := r.RunOnce(context.Background())
	if !errors.Is(err, yieldErr) || report.PagesProcessed != 1 {
		t.Fatalf("RunOnce() = (%+v, %v), want yield error after page one", report, err)
	}
	r = newReconciler(t, db, client, q, Options{})
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := requests(); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("requested pages = %v", got)
	}
}

func TestRunOnceContextCancellationPreservesCompletedPageCheckpoint(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	client, requests := paginatedPaperless(t, 2, 0)
	ctx, cancel := context.WithCancel(context.Background())
	r := newReconciler(t, db, client, q, Options{
		MaxArchivePagesPerPass: 2,
		Yield: func(ctx context.Context) error {
			cancel()
			return ctx.Err()
		},
	})

	report, err := r.RunOnce(ctx)
	if !errors.Is(err, context.Canceled) || report.PagesProcessed != 1 {
		t.Fatalf("RunOnce() = (%+v, %v), want cancellation after page one", report, err)
	}
	r = newReconciler(t, db, client, q, Options{})
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := requests(); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("requested pages = %v", got)
	}
}

func TestRunOnceCancellationDuringArchiveEnqueueDoesNotAdvanceCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconcile.db")
	db, q := openStore(t, path)
	lockDB, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lockDB.Close() })
	lockConn, err := lockDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lockConn.ExecContext(context.Background(), "ROLLBACK")
		lockConn.Close()
	})
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, `{"count":1,"next":"?page=2","results":[{"id":1,"checksum":"checksum","tags":[]}]}`)
	})
	r := newReconciler(t, db, client, q, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		report Report
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		report, err := r.RunOnce(ctx)
		resultCh <- result{report: report, err: err}
	}()
	waitForDBConnectionInUse(t, db)
	cancel()
	var got result
	select {
	case got = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("RunOnce() did not return after cancellation")
	}
	if !errors.Is(got.err, context.Canceled) || got.report.PagesProcessed != 0 {
		t.Fatalf("RunOnce() = (%+v, %v), want canceled before checkpoint", got.report, got.err)
	}
	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := lockConn.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoCheckpoint(t, db)
	var jobs int
	if err := db.QueryRow("SELECT count(*) FROM jobs").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Errorf("jobs = %d, want 0", jobs)
	}
}

func TestRunOnceRejectsImmediatePaginationLoopWithoutRefetch(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	requests := 0
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		requests++
		io.WriteString(writer, `{"count":1,"next":"/api/documents/","results":[{"id":1,"checksum":"checksum","tags":[]}]}`)
	})
	r := newReconciler(t, db, client, q, Options{})

	report, err := r.RunOnce(context.Background())
	if errorCategory(err) != saferr.CategoryPaperless || report.PagesProcessed != 1 {
		t.Fatalf("RunOnce() = (%+v, %v), want pagination loop", report, err)
	}
	if _, err := r.RunOnce(context.Background()); errorCategory(err) != saferr.CategoryPaperless {
		t.Fatalf("second RunOnce() error = %v, want persisted pagination loop", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
}

func TestRunOnceDetectsPaginationCycleAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconcile.db")
	db, q := openStore(t, path)
	requests := make([]string, 0, 2)
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		page := request.URL.Query().Get("page")
		if page == "" {
			page = "A"
		}
		requests = append(requests, page)
		if page == "A" {
			io.WriteString(writer, `{"count":2,"next":"?page=B","results":[{"id":1,"checksum":"a","tags":[]}]}`)
			return
		}
		io.WriteString(writer, `{"count":2,"next":"/api/documents/","results":[{"id":2,"checksum":"b","tags":[]}]}`)
	})
	r := newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 1})
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q = queue.New(db)
	r = newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 1})
	if report, err := r.RunOnce(context.Background()); errorCategory(err) != saferr.CategoryPaperless || report.PagesProcessed != 1 {
		t.Fatalf("cycle RunOnce() = (%+v, %v)", report, err)
	}
	if _, err := r.RunOnce(context.Background()); errorCategory(err) != saferr.CategoryPaperless {
		t.Fatalf("blocked RunOnce() error = %v", err)
	}
	if !slices.Equal(requests, []string{"A", "B"}) {
		t.Errorf("requests = %v", requests)
	}
}

func TestRunOnceEnforcesCumulativePageLimitAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconcile.db")
	db, q := openStore(t, path)
	client, requests := paginatedPaperless(t, 4, 0)
	r := newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 1, MaxArchivePagesPerScan: 2})
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	q = queue.New(db)
	r = newReconciler(t, db, client, q, Options{MaxArchivePagesPerPass: 1, MaxArchivePagesPerScan: 2})
	if report, err := r.RunOnce(context.Background()); errorCategory(err) != saferr.CategoryPaperless || report.PagesProcessed != 1 {
		t.Fatalf("limit RunOnce() = (%+v, %v)", report, err)
	}
	if _, err := r.RunOnce(context.Background()); errorCategory(err) != saferr.CategoryPaperless {
		t.Fatalf("blocked RunOnce() error = %v", err)
	}
	if got := requests(); !slices.Equal(got, []string{"1", "2"}) {
		t.Errorf("requests = %v", got)
	}
}

func TestRunOnceRejectsMalformedCheckpointWithoutLeakingValue(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	sensitive := `{"version":99,"next":"https://example.test/?token=sensitive"}`
	if _, err := db.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)`, checkpointKey, sensitive, time.Now().UTC().Format(checkpointTimestampLayout)); err != nil {
		t.Fatal(err)
	}
	client, requests := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		t.Fatal("request made with malformed checkpoint")
	})
	r := newReconciler(t, db, client, q, Options{})

	_, err := r.RunOnce(context.Background())
	if errorCategory(err) != saferr.CategoryInternal {
		t.Fatalf("RunOnce() error = %v, want internal", err)
	}
	if strings.Contains(err.Error(), "sensitive") || len(requests()) != 0 {
		t.Errorf("malformed checkpoint error/request leaked: %q, %v", err, requests())
	}
}

func TestRunOnceErrorsDoNotExposeDocumentOrCursorData(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, `{"count":1,"next":"https://elsewhere.example/api/documents/?token=sensitive","results":[{"id":1,"checksum":"sensitive-checksum","tags":[]}]}`)
	})
	r := newReconciler(t, db, client, q, Options{})

	_, err := r.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce() error = nil, want unsafe cursor error")
	}
	if errorCategory(err) != saferr.CategoryPaperless {
		t.Errorf("RunOnce() category = %q, want paperless", errorCategory(err))
	}
	for _, forbidden := range []string{"sensitive-checksum", "elsewhere.example", "token=sensitive"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("RunOnce() error %q exposes %q", err, forbidden)
		}
	}
}

func TestNewValidatesDependenciesAndOptions(t *testing.T) {
	db, q := openStore(t, filepath.Join(t.TempDir(), "reconcile.db"))
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {})
	for _, options := range []Options{
		{Model: " ", PromptVersion: "v1"},
		{Model: "model", PromptVersion: " "},
		{Model: "model", PromptVersion: "v1", MaxCandidatesPerPass: -1},
		{Model: "model", PromptVersion: "v1", MaxArchivePagesPerPass: -1},
		{Model: "model", PromptVersion: "v1", MaxArchivePagesPerScan: -1},
	} {
		if _, err := New(db, client, q, options); err == nil {
			t.Errorf("New(%+v) error = nil", options)
		}
	}
}

func openStore(t *testing.T, path string) (*sql.DB, *queue.Queue) {
	t.Helper()
	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, queue.New(db)
}

func newReconciler(t *testing.T, db *sql.DB, client *paperless.Client, q *queue.Queue, options Options) *Reconciler {
	t.Helper()
	if options.Model == "" {
		options.Model = "model"
	}
	if options.PromptVersion == "" {
		options.PromptVersion = "v1"
	}
	r, err := New(db, client, q, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r
}

func testPaperless(t *testing.T, handler http.HandlerFunc) (*paperless.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.URL.Path)
		mu.Unlock()
		handler(writer, request)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client, err := paperless.New(baseURL, "token", paperless.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
}

func paginatedPaperless(t *testing.T, pages, failOncePage int) (*paperless.Client, func() []string) {
	t.Helper()
	failed := false
	var mu sync.Mutex
	var requested []string
	client, _ := testPaperless(t, func(writer http.ResponseWriter, request *http.Request) {
		page := 1
		if _, err := fmt.Sscanf(request.URL.Query().Get("page"), "%d", &page); err != nil {
			page = 1
		}
		mu.Lock()
		requested = append(requested, fmt.Sprint(page))
		mu.Unlock()
		if page == failOncePage && !failed {
			failed = true
			http.Error(writer, "temporary", http.StatusInternalServerError)
			return
		}
		next := "null"
		if page < pages {
			next = fmt.Sprintf("%q", fmt.Sprintf("?page=%d", page+1))
		}
		fmt.Fprintf(writer, `{"count":%d,"next":%s,"results":[{"id":%d,"checksum":"checksum-%d","tags":[]}]}`, pages, next, page, page)
	})
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requested)
	}
}

func assertClaimOrder(t *testing.T, q *queue.Queue, want []int64) {
	t.Helper()
	for _, documentID := range want {
		job, ok, err := q.Claim("worker", time.Minute)
		if err != nil || !ok || job.DocumentID != documentID {
			t.Fatalf("Claim() = (%+v, %t, %v), want document %d", job, ok, err, documentID)
		}
		if err := q.Complete(job.ID, job.Attempts, "worker"); err != nil {
			t.Fatal(err)
		}
	}
}

func errorCategory(err error) saferr.Category {
	var safeError *saferr.Error
	if errors.As(err, &safeError) {
		return safeError.Category()
	}
	return ""
}

func waitForDBConnectionInUse(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if db.Stats().InUse == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("database operation did not start")
		case <-ticker.C:
		}
	}
}

func assertNoCheckpoint(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM settings WHERE key = ?", checkpointKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("checkpoint count = %d, want 0", count)
	}
}

func checkpointFingerprint(cursor string) string {
	sum := sha256.Sum256([]byte(cursor))
	return hex.EncodeToString(sum[:])
}

func loadCheckpointValue(t *testing.T, db *sql.DB) map[string]any {
	t.Helper()
	var value string
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", checkpointKey).Scan(&value); err != nil {
		t.Fatal(err)
	}
	var checkpoint map[string]any
	if err := json.Unmarshal([]byte(value), &checkpoint); err != nil {
		t.Fatal(err)
	}
	return checkpoint
}
