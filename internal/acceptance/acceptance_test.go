//go:build acceptance

package acceptance_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/app"
	"github.com/nosovk/paperless-ai-ocr/internal/config"
	"github.com/nosovk/paperless-ai-ocr/internal/observability"
	"github.com/nosovk/paperless-ai-ocr/internal/securelog"
	"github.com/nosovk/paperless-ai-ocr/internal/server"
	_ "modernc.org/sqlite"
)

const (
	webhookToken  = "acceptance-webhook-token"
	nativeContent = "native searchable evidence"
	probeNonce    = "OCR-PROBE-7K3M9Q2X"
)

var pageRangePattern = regexp.MustCompile(`pages ([0-9]+) through ([0-9]+)`)

func TestWebhookDirectPDFSuccessIsSearchable(t *testing.T) {
	environment := newEnvironment(t)
	environment.ai.capability = "direct"
	running := environment.start(t, serviceSpec{})
	document := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)

	postWebhook(t, running.url, document.ID)
	environment.paperless.waitContent(t, document.ID, "accepted page 1")

	if got := environment.paperless.search(t, "accepted page 1"); !slices.Equal(got, []int{document.ID}) {
		t.Fatalf("search results = %v, want [%d]", got, document.ID)
	}
	if got := environment.ai.transcriptions(); !slices.Equal(got, []aiCall{{documentID: document.ID, firstPage: 1, lastPage: 1, transport: "input_file"}}) {
		t.Fatalf("AI calls = %+v", got)
	}
	environment.paperlessAI.waitDispatches(t, []int{document.ID})
	running.stop(t)
}

func TestReconciliationBackfillImageFallbackAndWebhookPriority(t *testing.T) {
	environment := newEnvironment(t)
	environment.ai.capability = "direct"
	long := environment.paperless.addDocument(t, "six-page.pdf", nativeContent)
	backfill := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)
	environment.ai.blockDocument(long.ID)
	running := environment.start(t, serviceSpec{})
	environment.ai.waitStarted(t, long.ID, 1, 5)
	webhook := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)
	postWebhook(t, running.url, webhook.ID)
	environment.waitJobPriority(t, running.databasePath, webhook.ID, 100)
	environment.ai.releaseDocument(long.ID)

	environment.paperless.waitContent(t, long.ID, "accepted page 6")
	environment.paperless.waitContent(t, webhook.ID, "accepted page 1")
	environment.paperless.waitContent(t, backfill.ID, "accepted page 1")
	calls := environment.ai.transcriptions()
	if len(calls) != 4 {
		t.Fatalf("AI calls = %+v, want four batches", calls)
	}
	if calls[0].documentID != long.ID || calls[0].firstPage != 1 || calls[0].lastPage != 5 || calls[0].transport != "input_image" ||
		calls[1].documentID != long.ID || calls[1].firstPage != 6 || calls[1].lastPage != 6 || calls[1].transport != "input_image" {
		t.Fatalf("long document calls = %+v", calls[:2])
	}
	if calls[2].documentID != webhook.ID || calls[3].documentID != backfill.ID {
		t.Fatalf("post-active priority order = %+v, want webhook %d before backfill %d", calls[2:], webhook.ID, backfill.ID)
	}
	running.stop(t)
}

func TestRestartResumesLongDocumentWithoutRepeatingCheckpoint(t *testing.T) {
	environment := newEnvironment(t)
	document := environment.paperless.addDocument(t, "six-page.pdf", nativeContent)
	environment.ai.blockRange(document.ID, 6, 6)
	databasePath := filepath.Join(t.TempDir(), "restart.db")
	first := environment.start(t, serviceSpec{databasePath: databasePath})
	environment.ai.waitStarted(t, document.ID, 6, 6)
	first.stop(t)
	environment.ai.releaseRange(document.ID, 6, 6)
	second := environment.start(t, serviceSpec{databasePath: databasePath})
	environment.paperless.waitContent(t, document.ID, "accepted page 6")
	second.stop(t)

	if got := environment.ai.rangeAttempts(document.ID, 1, 5); got != 1 {
		t.Fatalf("first checkpoint attempts = %d, want 1", got)
	}
	if got := environment.ai.rangeAttempts(document.ID, 6, 6); got != 2 {
		t.Fatalf("interrupted range attempts = %d, want 2", got)
	}
}

func TestFailureAndRetrySafety(t *testing.T) {
	t.Run("checksum_race", func(t *testing.T) {
		environment := newEnvironment(t)
		document := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)
		environment.paperless.changeChecksumAfterDownload(document.ID)
		running := environment.start(t, serviceSpec{pollInterval: time.Hour})
		environment.waitAnyJobState(t, running.databasePath, document.ID, "failed")
		if got := environment.paperless.content(document.ID); !strings.Contains(got, nativeContent) {
			t.Fatalf("content = %q, want preserved native content", got)
		}
		if got := environment.paperlessAI.dispatches(); len(got) != 0 {
			t.Fatalf("dispatches = %v, want none", got)
		}
		running.stop(t)
	})

	t.Run("rate_limit", func(t *testing.T) {
		environment := newEnvironment(t)
		document := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)
		environment.ai.rateLimitOnce(document.ID)
		running := environment.start(t, serviceSpec{modelAttempts: 2})
		environment.paperless.waitContent(t, document.ID, "accepted page 1")
		if got := environment.ai.rangeAttempts(document.ID, 1, 1); got != 2 {
			t.Fatalf("transcription attempts = %d, want 2", got)
		}
		running.stop(t)
	})

	t.Run("terminal_failure", func(t *testing.T) {
		environment := newEnvironment(t)
		document := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)
		environment.ai.failTerminal(document.ID)
		running := environment.start(t, serviceSpec{})
		environment.waitJobState(t, running.databasePath, document.ID, "failed")
		if got := environment.paperless.content(document.ID); !strings.Contains(got, nativeContent) {
			t.Fatalf("content = %q, want preserved native content", got)
		}
		if !environment.paperless.hasTag(document.ID, "ai-ocr-failed") {
			t.Fatal("terminal failure did not add ai-ocr-failed")
		}
		if got := environment.paperlessAI.dispatches(); len(got) != 0 {
			t.Fatalf("dispatches = %v, want none", got)
		}
		running.stop(t)
	})

	t.Run("downstream_503_retry", func(t *testing.T) {
		environment := newEnvironment(t)
		document := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)
		environment.paperlessAI.serviceUnavailableOnce(document.ID)
		startedAt := time.Now()
		running := environment.start(t, serviceSpec{retryDelay: 500 * time.Millisecond})
		environment.paperlessAI.waitDispatch(t)
		nextAttemptAt := environment.waitJobRetry(t, running.databasePath, document.ID)
		if !nextAttemptAt.After(time.Now()) {
			t.Fatalf("next_attempt_at = %s, want a future time", nextAttemptAt)
		}
		if delay := nextAttemptAt.Sub(startedAt); delay < 400*time.Millisecond {
			t.Fatalf("next_attempt_at delay = %s, want at least 400ms", delay)
		}
		checkAt := nextAttemptAt.Add(-100 * time.Millisecond)
		if wait := time.Until(checkAt); wait <= 0 {
			t.Fatalf("retry observation left no pre-deadline check window: next_attempt_at = %s", nextAttemptAt)
		} else {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			<-timer.C
		}
		if got := environment.paperlessAI.dispatches(); !slices.Equal(got, []int{document.ID}) {
			t.Fatalf("dispatches before next_attempt_at = %v, want one attempt", got)
		}
		if got := environment.paperless.updateCount(document.ID); got != 1 {
			t.Fatalf("content updates before next_attempt_at = %d, want 1", got)
		}
		if got := environment.ai.rangeAttempts(document.ID, 1, 1); got != 1 {
			t.Fatalf("transcription attempts before next_attempt_at = %d, want 1", got)
		}
		environment.waitJobState(t, running.databasePath, document.ID, "completed")
		if got := environment.paperless.updateCount(document.ID); got != 1 {
			t.Fatalf("content updates = %d, want 1", got)
		}
		if got := environment.ai.rangeAttempts(document.ID, 1, 1); got != 1 {
			t.Fatalf("transcription attempts = %d, want 1", got)
		}
		if got := environment.paperlessAI.dispatches(); !slices.Equal(got, []int{document.ID, document.ID}) {
			t.Fatalf("dispatches = %v, want two attempts", got)
		}
		running.stop(t)
	})
}

func TestCompletedDocumentIgnoresModelAndPromptOnlyChanges(t *testing.T) {
	environment := newEnvironment(t)
	document := environment.paperless.addDocument(t, "one-page.pdf", nativeContent)
	databasePath := filepath.Join(t.TempDir(), "completed.db")
	first := environment.start(t, serviceSpec{databasePath: databasePath, model: "model-a", promptVersion: "prompt-a"})
	environment.waitJobState(t, databasePath, document.ID, "completed")
	first.stop(t)
	transcriptions := len(environment.ai.transcriptions())
	dispatches := len(environment.paperlessAI.dispatches())

	second := environment.start(t, serviceSpec{databasePath: databasePath, model: "model-b", promptVersion: "prompt-b"})
	waitFor(t, func() bool { return second.readiness.Ready() }, "second service readiness")
	if got := jobCount(t, databasePath, document.ID); got != 1 {
		t.Fatalf("jobs after model/prompt change = %d, want 1", got)
	}
	second.stop(t)
	if got := len(environment.ai.transcriptions()); got != transcriptions {
		t.Fatalf("transcriptions after model/prompt change = %d, want %d", got, transcriptions)
	}
	if got := len(environment.paperlessAI.dispatches()); got != dispatches {
		t.Fatalf("dispatches after model/prompt change = %d, want %d", got, dispatches)
	}
}

type environment struct {
	paperless   *fakePaperless
	ai          *fakeAIGate
	paperlessAI *fakePaperlessAI
}

func newEnvironment(t *testing.T) *environment {
	t.Helper()
	paperless := newFakePaperless(t)
	ai := newFakeAIGate(t)
	paperlessAI := newFakePaperlessAI(t)
	t.Cleanup(paperless.server.Close)
	t.Cleanup(ai.server.Close)
	t.Cleanup(paperlessAI.server.Close)
	return &environment{paperless: paperless, ai: ai, paperlessAI: paperlessAI}
}

type serviceSpec struct {
	databasePath  string
	model         string
	promptVersion string
	modelAttempts int
	pollInterval  time.Duration
	retryDelay    time.Duration
}

type runningService struct {
	url          string
	databasePath string
	readiness    *server.Readiness
	cancel       context.CancelCauseFunc
	done         chan error
}

func (environment *environment) start(t *testing.T, spec serviceSpec) *runningService {
	t.Helper()
	if spec.databasePath == "" {
		spec.databasePath = filepath.Join(t.TempDir(), "service.db")
	}
	if spec.model == "" {
		spec.model = "acceptance-model"
	}
	if spec.promptVersion == "" {
		spec.promptVersion = "acceptance-prompt"
	}
	if spec.modelAttempts == 0 {
		spec.modelAttempts = 1
	}
	if spec.pollInterval == 0 {
		spec.pollInterval = 20 * time.Millisecond
	}
	if spec.retryDelay == 0 {
		spec.retryDelay = 10 * time.Millisecond
	}
	readiness := server.NewReadiness()
	metrics := observability.NewMetrics()
	cfg := config.Config{
		PaperlessURL: mustURL(t, environment.paperless.server.URL+"/"), PaperlessAPIToken: "paperless-token",
		AIBaseURL: mustURL(t, environment.ai.server.URL+"/"), AIAPIKey: "ai-key", AIModel: spec.model,
		WebhookToken: webhookToken, PaperlessAIWebhookURL: mustURL(t, environment.paperlessAI.server.URL+"/dispatch"),
		PaperlessAIWebhookKey: "paperless-ai-key", RenderDPI: 72, BatchSize: 5,
		ModelAttempts: spec.modelAttempts, RenderTimeout: 10 * time.Second, ModelTimeout: 10 * time.Second,
		DocumentDeadline: time.Minute, TemporaryRenderBudget: 64 << 20,
	}
	service, err := app.NewServiceWithOptions(cfg, readiness, metrics, app.ServiceOptions{
		DatabasePath: spec.databasePath, RetryDelay: spec.retryDelay, PromptVersion: spec.promptVersion,
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx, app.Options{
			Runtime: service.Runtime, Readiness: readiness, Metrics: metrics, Handler: service.Handler,
			Listener: listener, HTTPServer: &http.Server{}, PollInterval: spec.pollInterval,
			IdleInterval: 2 * time.Millisecond, ShutdownTimeout: 5 * time.Second, Logger: securelog.New(io.Discard),
		})
	}()
	running := &runningService{url: "http://" + listener.Addr().String(), databasePath: spec.databasePath, readiness: readiness, cancel: cancel, done: done}
	waitFor(t, func() bool { return readiness.Ready() }, "service readiness")
	return running
}

func (running *runningService) stop(t *testing.T) {
	t.Helper()
	running.cancel(context.Canceled)
	select {
	case err := <-running.done:
		if err != nil {
			t.Fatalf("app.Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out stopping service")
	}
}

func (environment *environment) waitJobState(t *testing.T, databasePath string, documentID int, state string) {
	t.Helper()
	waitFor(t, func() bool {
		db, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return false
		}
		defer db.Close()
		var got string
		return db.QueryRow("SELECT state FROM jobs WHERE document_id = ? ORDER BY id DESC LIMIT 1", documentID).Scan(&got) == nil && got == state
	}, "job state "+state)
}

func (environment *environment) waitJobRetry(t *testing.T, databasePath string, documentID int) time.Time {
	t.Helper()
	var nextAttemptAt time.Time
	waitFor(t, func() bool {
		db, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return false
		}
		defer db.Close()
		var state, rawNextAttemptAt string
		if err := db.QueryRow("SELECT state, available_at AS next_attempt_at FROM jobs WHERE document_id = ? ORDER BY id DESC LIMIT 1", documentID).Scan(&state, &rawNextAttemptAt); err != nil || state != "retry" {
			return false
		}
		nextAttemptAt, err = time.Parse(time.RFC3339Nano, rawNextAttemptAt)
		return err == nil
	}, "durable retry state")
	return nextAttemptAt
}

func (environment *environment) waitAnyJobState(t *testing.T, databasePath string, documentID int, state string) {
	t.Helper()
	waitFor(t, func() bool {
		db, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return false
		}
		defer db.Close()
		var count int
		return db.QueryRow("SELECT count(*) FROM jobs WHERE document_id = ? AND state = ?", documentID, state).Scan(&count) == nil && count > 0
	}, "job state "+state)
}

func (environment *environment) waitJobPriority(t *testing.T, databasePath string, documentID, priority int) {
	t.Helper()
	waitFor(t, func() bool {
		db, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			return false
		}
		defer db.Close()
		var got int
		return db.QueryRow("SELECT priority FROM jobs WHERE document_id = ? AND state = 'pending' ORDER BY id DESC LIMIT 1", documentID).Scan(&got) == nil && got == priority
	}, "webhook priority job")
}

func jobCount(t *testing.T, databasePath string, documentID int) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM jobs WHERE document_id = ?", documentID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

type fakeDocument struct {
	ID                          int    `json:"id"`
	Content                     string `json:"content"`
	Checksum                    string `json:"checksum"`
	Tags                        []int  `json:"tags"`
	PDF                         []byte `json:"-"`
	updates                     int
	changeChecksumAfterDownload bool
}

type fakePaperless struct {
	server       *httptest.Server
	mu           sync.Mutex
	documents    map[int]*fakeDocument
	tags         map[int]string
	nextDocument int
	nextTag      int
}

func newFakePaperless(t *testing.T) *fakePaperless {
	t.Helper()
	fake := &fakePaperless{documents: make(map[int]*fakeDocument), tags: make(map[int]string), nextDocument: 1, nextTag: 1}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	return fake
}

func (fake *fakePaperless) addDocument(t *testing.T, fixture, content string) fakeDocument {
	t.Helper()
	directory := "acceptance"
	if fixture == "one-page.pdf" {
		directory = "pdfs"
	}
	pdf, err := os.ReadFile(filepath.Join("..", "..", "testdata", directory, fixture))
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	id := fake.nextDocument
	fake.nextDocument++
	document := &fakeDocument{ID: id, Content: fmt.Sprintf("%s\n[acceptance-document-id:%d]", content, id), Checksum: fmt.Sprintf("checksum-%d", id), Tags: []int{}, PDF: pdf}
	fake.documents[id] = document
	return *document
}

func (fake *fakePaperless) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/api/":
		writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
	case request.Method == http.MethodGet && request.URL.Path == "/api/documents/":
		fake.listDocuments(writer)
	case request.Method == http.MethodGet && request.URL.Path == "/api/search/":
		fake.searchDocuments(writer, request.URL.Query().Get("query"))
	case strings.HasPrefix(request.URL.Path, "/api/documents/"):
		fake.documentRequest(writer, request)
	case request.URL.Path == "/api/tags/":
		fake.tagsRequest(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (fake *fakePaperless) listDocuments(writer http.ResponseWriter) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	results := make([]*fakeDocument, 0, len(fake.documents))
	for _, document := range fake.documents {
		clone := *document
		clone.PDF = nil
		results = append(results, &clone)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	writeJSON(writer, http.StatusOK, map[string]any{"count": len(results), "next": nil, "previous": nil, "results": results})
}

func (fake *fakePaperless) documentRequest(writer http.ResponseWriter, request *http.Request) {
	remainder := strings.TrimPrefix(request.URL.Path, "/api/documents/")
	download := strings.HasSuffix(remainder, "/download/")
	idText := strings.TrimSuffix(strings.TrimSuffix(remainder, "download/"), "/")
	id, err := strconv.Atoi(idText)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	fake.mu.Lock()
	document := fake.documents[id]
	if document == nil {
		fake.mu.Unlock()
		http.NotFound(writer, request)
		return
	}
	switch {
	case request.Method == http.MethodGet && download:
		data := bytes.Clone(document.PDF)
		if document.changeChecksumAfterDownload {
			document.Checksum += "-changed"
			document.changeChecksumAfterDownload = false
		}
		fake.mu.Unlock()
		writer.Header().Set("Content-Type", "application/pdf")
		_, _ = writer.Write(data)
	case request.Method == http.MethodGet:
		clone := *document
		clone.PDF = nil
		fake.mu.Unlock()
		writeJSON(writer, http.StatusOK, clone)
	case request.Method == http.MethodPatch:
		var patch struct {
			Content *string `json:"content"`
			Tags    *[]int  `json:"tags"`
		}
		err := json.NewDecoder(request.Body).Decode(&patch)
		if err == nil && patch.Content != nil {
			document.Content = *patch.Content
			document.updates++
		}
		if err == nil && patch.Tags != nil {
			document.Tags = slices.Clone(*patch.Tags)
		}
		clone := *document
		fake.mu.Unlock()
		if err != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		writeJSON(writer, http.StatusOK, clone)
	default:
		fake.mu.Unlock()
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (fake *fakePaperless) tagsRequest(writer http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if request.Method == http.MethodGet {
		name := request.URL.Query().Get("name__iexact")
		results := make([]map[string]any, 0, 1)
		for id, current := range fake.tags {
			if current == name {
				results = append(results, map[string]any{"id": id, "name": current})
			}
		}
		writeJSON(writer, http.StatusOK, map[string]any{"count": len(results), "next": nil, "previous": nil, "results": results})
		return
	}
	if request.Method == http.MethodPost {
		var input struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(request.Body).Decode(&input) != nil {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		id := fake.nextTag
		fake.nextTag++
		fake.tags[id] = input.Name
		writeJSON(writer, http.StatusCreated, map[string]any{"id": id, "name": input.Name})
		return
	}
	writer.WriteHeader(http.StatusMethodNotAllowed)
}

func (fake *fakePaperless) searchDocuments(writer http.ResponseWriter, query string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	ids := []int{}
	for id, document := range fake.documents {
		if strings.Contains(document.Content, query) {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	writeJSON(writer, http.StatusOK, map[string]any{"results": ids})
}

func (fake *fakePaperless) search(t *testing.T, query string) []int {
	t.Helper()
	response, err := http.Get(fake.server.URL + "/api/search/?query=" + url.QueryEscape(query))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct {
		Results []int `json:"results"`
	}
	if json.NewDecoder(response.Body).Decode(&result) != nil {
		t.Fatal("cannot decode fake Paperless search")
	}
	return result.Results
}

func (fake *fakePaperless) waitContent(t *testing.T, id int, text string) {
	t.Helper()
	waitFor(t, func() bool { return strings.Contains(fake.content(id), text) }, "Paperless content update")
}

func (fake *fakePaperless) content(id int) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.documents[id].Content
}

func (fake *fakePaperless) updateCount(id int) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.documents[id].updates
}

func (fake *fakePaperless) changeChecksumAfterDownload(id int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.documents[id].changeChecksumAfterDownload = true
}

func (fake *fakePaperless) hasTag(id int, name string) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, tagID := range fake.documents[id].Tags {
		if fake.tags[tagID] == name {
			return true
		}
	}
	return false
}

type aiCall struct {
	documentID int
	firstPage  int
	lastPage   int
	transport  string
}

type fakeAIGate struct {
	server      *httptest.Server
	mu          sync.Mutex
	capability  string
	calls       []aiCall
	attempts    map[string]int
	blocked     map[string]chan struct{}
	rateLimited map[int]bool
	terminal    map[int]bool
}

func newFakeAIGate(t *testing.T) *fakeAIGate {
	t.Helper()
	fake := &fakeAIGate{capability: "image", attempts: make(map[string]int), blocked: make(map[string]chan struct{}), rateLimited: make(map[int]bool), terminal: make(map[int]bool)}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	return fake
}

func (fake *fakeAIGate) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Input []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
		Text json.RawMessage `json:"text"`
	}
	if json.NewDecoder(request.Body).Decode(&body) != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	if len(body.Text) == 0 {
		fake.probe(writer, body.Input)
		return
	}
	var prompt string
	var transport string
	for _, input := range body.Input {
		for _, content := range input.Content {
			if content.Type == "input_text" && strings.Contains(content.Text, "Transcribe attached visual document pages") {
				prompt = content.Text
			}
			if content.Type == "input_file" || content.Type == "input_image" {
				transport = content.Type
			}
		}
	}
	match := pageRangePattern.FindStringSubmatch(prompt)
	if len(match) != 3 {
		http.Error(writer, "bad prompt", http.StatusBadRequest)
		return
	}
	first, _ := strconv.Atoi(match[1])
	last, _ := strconv.Atoi(match[2])
	documentID := documentIDFromPrompt(prompt)
	key := rangeKey(documentID, first, last)
	fake.mu.Lock()
	fake.calls = append(fake.calls, aiCall{documentID: documentID, firstPage: first, lastPage: last, transport: transport})
	fake.attempts[key]++
	attempt := fake.attempts[key]
	blocked := fake.blocked[key]
	rateLimited := fake.rateLimited[documentID]
	terminal := fake.terminal[documentID]
	fake.mu.Unlock()
	if blocked != nil {
		select {
		case <-blocked:
		case <-request.Context().Done():
			return
		}
	}
	if rateLimited && attempt == 1 {
		writer.Header().Set("Retry-After", "0")
		writeJSON(writer, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"type": "rate_limit"}})
		return
	}
	if terminal {
		writeAIOutput(writer, `{"pages":[]}`)
		return
	}
	pages := make([]map[string]any, 0, last-first+1)
	for page := first; page <= last; page++ {
		pages = append(pages, map[string]any{"page": page, "text": fmt.Sprintf("accepted page %d document %d", page, documentID), "refused": false})
	}
	raw, _ := json.Marshal(map[string]any{"pages": pages})
	writeAIOutput(writer, string(raw))
}

func (fake *fakeAIGate) probe(writer http.ResponseWriter, inputs []struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}) {
	transport := ""
	for _, input := range inputs {
		for _, content := range input.Content {
			if content.Type == "input_file" || content.Type == "input_image" {
				transport = content.Type
			}
		}
	}
	fake.mu.Lock()
	capability := fake.capability
	fake.mu.Unlock()
	if capability == "image" && transport == "input_file" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "unsupported_value", "param": "input[0].content[1].file_data", "message": "unsupported"}})
		return
	}
	writeAIOutput(writer, probeNonce)
}

func documentIDFromPrompt(prompt string) int {
	const marker = "[acceptance-document-id:"
	start := strings.Index(prompt, marker)
	if start == -1 {
		return 0
	}
	start += len(marker)
	end := strings.IndexByte(prompt[start:], ']')
	if end == -1 {
		return 0
	}
	id, _ := strconv.Atoi(prompt[start : start+end])
	return id
}

func (fake *fakeAIGate) blockDocument(id int) { fake.blockRange(id, 1, 5) }

func (fake *fakeAIGate) releaseDocument(id int) { fake.releaseRange(id, 1, 5) }

func (fake *fakeAIGate) blockRange(id, first, last int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.blocked[rangeKey(id, first, last)] = make(chan struct{})
}

func (fake *fakeAIGate) releaseRange(id, first, last int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	key := rangeKey(id, first, last)
	if channel := fake.blocked[key]; channel != nil {
		close(channel)
		delete(fake.blocked, key)
	}
}

func (fake *fakeAIGate) waitStarted(t *testing.T, id, first, last int) {
	t.Helper()
	waitFor(t, func() bool { return fake.rangeAttempts(id, first, last) > 0 }, "AI transcription start")
}

func (fake *fakeAIGate) rateLimitOnce(id int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.rateLimited[id] = true
}

func (fake *fakeAIGate) failTerminal(id int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.terminal[id] = true
}

func (fake *fakeAIGate) rangeAttempts(id, first, last int) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.attempts[rangeKey(id, first, last)]
}

func (fake *fakeAIGate) transcriptions() []aiCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return slices.Clone(fake.calls)
}

func rangeKey(id, first, last int) string { return fmt.Sprintf("%d:%d:%d", id, first, last) }

type fakePaperlessAI struct {
	server      *httptest.Server
	mu          sync.Mutex
	dispatched  []int
	unavailable map[int]bool
	dispatch    chan struct{}
}

func newFakePaperlessAI(t *testing.T) *fakePaperlessAI {
	t.Helper()
	fake := &fakePaperlessAI{unavailable: make(map[int]bool), dispatch: make(chan struct{}, 1)}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	return fake
}

func (fake *fakePaperlessAI) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if json.NewDecoder(request.Body).Decode(&body) != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	parts := strings.Split(strings.Trim(body.URL, "/"), "/")
	id, _ := strconv.Atoi(parts[len(parts)-1])
	fake.mu.Lock()
	fake.dispatched = append(fake.dispatched, id)
	unavailable := fake.unavailable[id]
	if unavailable {
		delete(fake.unavailable, id)
	}
	fake.mu.Unlock()
	select {
	case fake.dispatch <- struct{}{}:
	default:
	}
	if unavailable {
		writer.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writer.WriteHeader(http.StatusAccepted)
}

func (fake *fakePaperlessAI) serviceUnavailableOnce(id int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.unavailable[id] = true
}

func (fake *fakePaperlessAI) dispatches() []int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return slices.Clone(fake.dispatched)
}

func (fake *fakePaperlessAI) waitDispatch(t *testing.T) {
	t.Helper()
	select {
	case <-fake.dispatch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Paperless AI dispatch")
	}
}

func (fake *fakePaperlessAI) waitDispatches(t *testing.T, want []int) {
	t.Helper()
	waitFor(t, func() bool { return slices.Equal(fake.dispatches(), want) }, "Paperless AI dispatch")
}

func postWebhook(t *testing.T, baseURL string, documentID int) {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"document_id":%d}`, documentID))
	request, err := http.NewRequest(http.MethodPost, baseURL+"/webhooks/paperless", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+webhookToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook status = %d, want 202", response.StatusCode)
	}
}

func writeAIOutput(writer http.ResponseWriter, text string) {
	writeJSON(writer, http.StatusOK, map[string]any{"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": text}}}}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
