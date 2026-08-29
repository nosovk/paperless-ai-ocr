package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nosovk/paperless-ai-ocr/internal/database"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
)

const testToken = "correct-secret"

type enqueueCall struct {
	documentID int64
	priority   queue.Priority
}

type spyEnqueuer struct {
	mu    sync.Mutex
	calls []enqueueCall
	err   error
}

func (e *spyEnqueuer) EnqueueCandidate(_ context.Context, documentID int64, priority queue.Priority) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, enqueueCall{documentID: documentID, priority: priority})
	return len(e.calls) == 1, e.err
}

func (e *spyEnqueuer) snapshot() []enqueueCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]enqueueCall(nil), e.calls...)
}

func TestNewValidatesConfiguration(t *testing.T) {
	for _, test := range []struct {
		name     string
		token    string
		enqueuer CandidateEnqueuer
	}{
		{name: "blank token", token: " ", enqueuer: &spyEnqueuer{}},
		{name: "token with space", token: "two words", enqueuer: &spyEnqueuer{}},
		{name: "token with tab", token: "two\twords", enqueuer: &spyEnqueuer{}},
		{name: "token with comma", token: "two,words", enqueuer: &spyEnqueuer{}},
		{name: "token with control", token: "control\x1f", enqueuer: &spyEnqueuer{}},
		{name: "token with non ASCII", token: "caf\u00e9", enqueuer: &spyEnqueuer{}},
		{name: "nil enqueuer", token: testToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.token, test.enqueuer); err == nil {
				t.Fatal("New() error = nil, want configuration error")
			}
		})
	}
}

func TestWebhookRejectsUnauthorizedRequestsUniformly(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
	}{
		{name: "missing"},
		{name: "wrong", headers: []string{"Bearer authorization-canary"}},
		{name: "scheme only", headers: []string{"Bearer"}},
		{name: "wrong scheme", headers: []string{"Basic abc"}},
		{name: "blank credential", headers: []string{"Bearer  "}},
		{name: "leading space", headers: []string{" Bearer correct-secret"}},
		{name: "trailing space", headers: []string{"Bearer correct-secret "}},
		{name: "multiple spaces", headers: []string{"Bearer  correct-secret"}},
		{name: "tab separator", headers: []string{"Bearer\tcorrect-secret"}},
		{name: "tab in credential", headers: []string{"Bearer correct\tsecret"}},
		{name: "carriage return in credential", headers: []string{"Bearer correct\rsecret"}},
		{name: "line feed in credential", headers: []string{"Bearer correct\nsecret"}},
		{name: "comma in credential", headers: []string{"Bearer correct,secret"}},
		{name: "combined values", headers: []string{"Bearer correct-secret,Bearer correct-secret"}},
		{name: "control in credential", headers: []string{"Bearer correct\x1fsecret"}},
		{name: "non ASCII credential", headers: []string{"Bearer caf\u00e9"}},
		{name: "extra field", headers: []string{"Bearer correct-secret extra"}},
		{name: "multiple headers", headers: []string{"Bearer correct-secret", "Bearer correct-secret"}},
	}
	var wantBody string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enqueuer := &spyEnqueuer{}
			mux := newTestMux(t, testToken, enqueuer)
			request := httptest.NewRequest(http.MethodPost, "/webhooks/paperless", strings.NewReader(`{"document_id":123}`))
			request.Header.Set("Content-Type", "application/json")
			for _, header := range test.headers {
				request.Header.Add("Authorization", header)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			if got := response.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", got)
			}
			if wantBody == "" {
				wantBody = response.Body.String()
			}
			if response.Body.String() != wantBody {
				t.Errorf("body = %q, want uniform %q", response.Body.String(), wantBody)
			}
			if strings.Contains(response.Body.String(), "authorization-canary") || strings.Contains(response.Body.String(), testToken) {
				t.Errorf("response exposes authorization data: %q", response.Body.String())
			}
			if calls := enqueuer.snapshot(); len(calls) != 0 {
				t.Errorf("enqueue calls = %v, want none", calls)
			}
		})
	}
}

func TestWebhookAcceptsCaseInsensitiveBearerScheme(t *testing.T) {
	enqueuer := &spyEnqueuer{}
	response := performWebhook(t, newTestMux(t, testToken, enqueuer), "bEaReR "+testToken, "application/json; charset=utf-8", `{"document_id":123}`)
	if response.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", response.Code, http.StatusAccepted)
	}
}

func TestVerifyBearerUsesFixedSizeComparison(t *testing.T) {
	expected := hashToken(testToken)
	for _, credential := range []string{"x", testToken, strings.Repeat("x", 1000)} {
		called := false
		matched := verifyBearerToken("Bearer "+credential, expected, func(left, right []byte) int {
			called = true
			if len(left) != 32 || len(right) != 32 {
				t.Errorf("comparison lengths = (%d, %d), want (32, 32)", len(left), len(right))
			}
			if string(left) == string(right) {
				return 1
			}
			return 0
		})
		if !called {
			t.Errorf("credential length %d did not reach comparator", len(credential))
		}
		if matched != (credential == testToken) {
			t.Errorf("credential length %d match = %t", len(credential), matched)
		}
	}
}

func TestVerifyBearerRejectsMalformedSyntaxBeforeComparison(t *testing.T) {
	expected := hashToken(testToken)
	for _, authorization := range []string{
		"", "Bearer", "Bearer ", " Bearer token", "Bearer token ",
		"Bearer  token", "Bearer\ttoken", "Bearer to\tken", "Bearer token,other",
		"Bearer to\rken", "Bearer to\nken", "Bearer to\x1fken", "Bearer caf\u00e9", "Basic token",
	} {
		called := false
		if verifyBearerToken(authorization, expected, func(_, _ []byte) int {
			called = true
			return 1
		}) {
			t.Errorf("verifyBearerToken(%q) = true, want false", authorization)
		}
		if called {
			t.Errorf("verifyBearerToken(%q) called comparator", authorization)
		}
	}
}

func TestWebhookRejectsInvalidContentTypeAndJSON(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
	}{
		{name: "missing content type", body: `{"document_id":123}`, status: http.StatusUnsupportedMediaType},
		{name: "wrong content type", contentType: "text/plain", body: `{"document_id":123}`, status: http.StatusUnsupportedMediaType},
		{name: "malformed content type", contentType: "application/json; charset", body: `{"document_id":123}`, status: http.StatusUnsupportedMediaType},
		{name: "empty", contentType: "application/json", status: http.StatusBadRequest},
		{name: "malformed", contentType: "application/json", body: `{`, status: http.StatusBadRequest},
		{name: "empty object", contentType: "application/json", body: `{}`, status: http.StatusBadRequest},
		{name: "missing key", contentType: "application/json", body: `{"other":123}`, status: http.StatusBadRequest},
		{name: "duplicate key", contentType: "application/json", body: `{"document_id":123,"document_id":123}`, status: http.StatusBadRequest},
		{name: "duplicate key different value", contentType: "application/json", body: `{"document_id":123,"document_id":456}`, status: http.StatusBadRequest},
		{name: "uppercase key", contentType: "application/json", body: `{"DOCUMENT_ID":123}`, status: http.StatusBadRequest},
		{name: "mixed case key", contentType: "application/json", body: `{"Document_Id":123}`, status: http.StatusBadRequest},
		{name: "null ID", contentType: "application/json", body: `{"document_id":null}`, status: http.StatusBadRequest},
		{name: "array top level", contentType: "application/json", body: `[123]`, status: http.StatusBadRequest},
		{name: "number top level", contentType: "application/json", body: `123`, status: http.StatusBadRequest},
		{name: "null top level", contentType: "application/json", body: `null`, status: http.StatusBadRequest},
		{name: "trailing JSON", contentType: "application/json", body: `{"document_id":123}{}`, status: http.StatusBadRequest},
		{name: "trailing data", contentType: "application/json", body: `{"document_id":123}x`, status: http.StatusBadRequest},
		{name: "unknown field", contentType: "application/json", body: `{"document_id":123,"secret":"body-canary"}`, status: http.StatusBadRequest},
		{name: "string ID", contentType: "application/json", body: `{"document_id":"123"}`, status: http.StatusBadRequest},
		{name: "float ID", contentType: "application/json", body: `{"document_id":1.5}`, status: http.StatusBadRequest},
		{name: "zero ID", contentType: "application/json", body: `{"document_id":0}`, status: http.StatusBadRequest},
		{name: "negative ID", contentType: "application/json", body: `{"document_id":-1}`, status: http.StatusBadRequest},
		{name: "out of range ID", contentType: "application/json", body: `{"document_id":9223372036854775808}`, status: http.StatusBadRequest},
		{name: "oversized", contentType: "application/json", body: `{"document_id":123,"padding":"` + strings.Repeat("x", 4096) + `"}`, status: http.StatusRequestEntityTooLarge},
		{name: "oversized trailing content", contentType: "application/json", body: `{"document_id":123}` + strings.Repeat(" ", 4096), status: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enqueuer := &spyEnqueuer{}
			response := performWebhook(t, newTestMux(t, testToken, enqueuer), "Bearer "+testToken, test.contentType, test.body)
			if response.Code != test.status {
				t.Errorf("status = %d, want %d; body = %q", response.Code, test.status, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "body-canary") || strings.Contains(response.Body.String(), testToken) {
				t.Errorf("response exposes request data: %q", response.Body.String())
			}
			if calls := enqueuer.snapshot(); len(calls) != 0 {
				t.Errorf("enqueue calls = %v, want none", calls)
			}
		})
	}
}

func TestWebhookEnqueuesOnlyHighPriorityCandidateAndReturnsAccepted(t *testing.T) {
	enqueuer := &spyEnqueuer{}
	response := performWebhook(t, newTestMux(t, testToken, enqueuer), "Bearer "+testToken, "application/json", `{"document_id":123}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusAccepted, response.Body.String())
	}
	if response.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", response.Body.String())
	}
	if calls := enqueuer.snapshot(); fmt.Sprint(calls) != fmt.Sprint([]enqueueCall{{documentID: 123, priority: queue.PriorityWebhook}}) {
		t.Errorf("enqueue calls = %v, want document 123 at webhook priority", calls)
	}
}

func TestWebhookDuplicateDeliveryIsDurableAndIdempotent(t *testing.T) {
	db := openTestDatabase(t)
	mux := newTestMux(t, testToken, queue.New(db))
	for range 2 {
		response := performWebhook(t, mux, "Bearer "+testToken, "application/json", `{"document_id":123}`)
		if response.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d; body = %q", response.Code, http.StatusAccepted, response.Body.String())
		}
	}
	assertCandidate(t, db, 123, queue.PriorityWebhook, 1)
}

func TestConcurrentWebhookDuplicatesCreateOneCandidate(t *testing.T) {
	db := openTestDatabase(t)
	mux := newTestMux(t, testToken, queue.New(db))
	const deliveries = 20
	start := make(chan struct{})
	statuses := make(chan int, deliveries)
	var wg sync.WaitGroup
	for range deliveries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response := performWebhook(t, mux, "Bearer "+testToken, "application/json", `{"document_id":123}`)
			statuses <- response.Code
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted {
			t.Errorf("status = %d, want %d", status, http.StatusAccepted)
		}
	}
	assertCandidate(t, db, 123, queue.PriorityWebhook, 1)
}

func TestWebhookQueueFailureIsUnavailableAndSafe(t *testing.T) {
	enqueuer := &spyEnqueuer{err: errors.New("enqueue-error-canary")}
	response := performWebhook(t, newTestMux(t, testToken, enqueuer), "Bearer "+testToken, "application/json", `{"document_id":123}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if strings.Contains(response.Body.String(), "enqueue-error-canary") {
		t.Errorf("response exposes enqueue error: %q", response.Body.String())
	}
}

func TestWebhookRouteRejectsOtherMethods(t *testing.T) {
	mux := newTestMux(t, testToken, &spyEnqueuer{})
	request := httptest.NewRequest(http.MethodGet, "/webhooks/paperless", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if got := response.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func newTestMux(t *testing.T, token string, enqueuer CandidateEnqueuer) *http.ServeMux {
	t.Helper()
	mux, err := New(token, enqueuer)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return mux
}

func performWebhook(t *testing.T, handler http.Handler, authorization, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/webhooks/paperless", strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func openTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("database.Open(): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func assertCandidate(t *testing.T, db *sql.DB, documentID int64, priority queue.Priority, count int) {
	t.Helper()
	var gotCount int
	var gotPriority queue.Priority
	if err := db.QueryRow(`SELECT count(*), priority FROM candidates WHERE document_id = ?`, documentID).Scan(&gotCount, &gotPriority); err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if gotCount != count || gotPriority != priority {
		t.Errorf("candidate = (count %d, priority %d), want (%d, %d)", gotCount, gotPriority, count, priority)
	}
}
