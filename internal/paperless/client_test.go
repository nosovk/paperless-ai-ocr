package paperless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const testToken = "canary-paperless-token"

func TestNewValidatesConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		token   string
	}{
		{name: "nil URL", token: testToken},
		{name: "unsupported scheme", baseURL: "ftp://paperless.example", token: testToken},
		{name: "missing host", baseURL: "https:///paperless", token: testToken},
		{name: "userinfo", baseURL: "https://user@paperless.example", token: testToken},
		{name: "fragment", baseURL: "https://paperless.example/#secret", token: testToken},
		{name: "blank token", baseURL: "https://paperless.example", token: "  "},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var baseURL *url.URL
			if test.baseURL != "" {
				baseURL, _ = url.Parse(test.baseURL)
			}
			_, err := New(baseURL, test.token, Options{})
			if err == nil {
				t.Fatal("New() error = nil, want error")
			}
		})
	}
}

func TestPingUsesBoundedAPIRootRequest(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/paperless/api/" || request.URL.RawQuery != "" {
			t.Errorf("request = %s %s", request.Method, request.URL.RequestURI())
		}
		if got, want := request.Header.Get("Authorization"), "Token "+testToken; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		writer.WriteHeader(http.StatusOK)
		io.WriteString(writer, strings.Repeat("x", 8192))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL+"/paperless", Options{})
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestPingRejectsFailureWithoutResponseBody(t *testing.T) {
	const canary = "CANARY private Paperless response"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, canary, http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL, Options{})
	err := client.Ping(context.Background())
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("Ping() error = %v, want safe failure", err)
	}
}

func TestWalkDocumentsFollowsRelativeAndAbsolutePagination(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.Header.Get("Authorization"), "Token "+testToken; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		switch request.URL.Query().Get("page") {
		case "":
			fmt.Fprintf(writer, `{"count":3,"next":"?page=2","previous":null,"results":[{"id":1,"checksum":"one","tags":[3]}]}`)
		case "2":
			fmt.Fprintf(writer, `{"count":3,"next":%q,"previous":"?page=1","results":[{"id":2,"checksum":"two","tags":[]}]}`, server.URL+"/paperless/api/documents/?page=3")
		case "3":
			io.WriteString(writer, `{"count":3,"next":null,"previous":"?page=2","results":[{"id":3,"checksum":"three","tags":[4,5]}]}`)
		default:
			http.Error(writer, "unexpected page", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL+"/paperless", Options{})
	var pages [][]Document
	err := client.WalkDocuments(context.Background(), func(documents []Document) error {
		pages = append(pages, slices.Clone(documents))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDocuments() error = %v", err)
	}
	if got, want := pages, [][]Document{{{ID: 1, Checksum: "one", Tags: []int{3}}}, {{ID: 2, Checksum: "two", Tags: []int{}}}, {{ID: 3, Checksum: "three", Tags: []int{4, 5}}}}; !slices.EqualFunc(got, want, documentsEqual) {
		t.Errorf("WalkDocuments() pages = %#v, want %#v", got, want)
	}
}

func TestListDocumentsPageReturnsValidatedOpaqueCursor(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") == "2" {
			io.WriteString(writer, `{"count":2,"next":null,"results":[{"id":2,"checksum":"two","tags":[]}]}`)
			return
		}
		fmt.Fprintf(writer, `{"count":2,"next":%q,"results":[{"id":1,"checksum":"one","tags":[]}]}`, "?page=2")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/", Options{})

	first, err := client.ListDocumentsPage(context.Background(), "")
	if err != nil {
		t.Fatalf("first ListDocumentsPage() error = %v", err)
	}
	if len(first.Documents) != 1 || first.Documents[0].ID != 1 || first.Next == "" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := client.ListDocumentsPage(context.Background(), first.Next)
	if err != nil {
		t.Fatalf("second ListDocumentsPage() error = %v", err)
	}
	if len(second.Documents) != 1 || second.Documents[0].ID != 2 || second.Next != "" {
		t.Fatalf("second page = %+v", second)
	}
}

func TestListDocumentsPageRejectsTamperedCursorAndUnsafeNext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, `{"count":1,"next":"https://elsewhere.example/api/documents/","results":[]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/paperless/", Options{})

	assertPaperlessError(t, func() error {
		_, err := client.ListDocumentsPage(context.Background(), server.URL+"/api/documents/?page=2")
		return err
	}())
	assertPaperlessError(t, func() error {
		_, err := client.ListDocumentsPage(context.Background(), "")
		return err
	}())
}

func TestWalkDocumentsRejectsUnsafePagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		maxPages int
		next     func(*httptest.Server) string
	}{
		{name: "loop", next: func(server *httptest.Server) string { return server.URL + "/api/documents/" }},
		{name: "cross origin", next: func(_ *httptest.Server) string { return "https://elsewhere.example/api/documents/" }},
		{name: "page limit", maxPages: 1, next: func(server *httptest.Server) string { return server.URL + "/api/documents/?page=2" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(writer, `{"count":1,"next":%q,"results":[]}`, test.next(server))
			}))
			t.Cleanup(server.Close)

			client := newTestClient(t, server.URL, Options{MaxPages: test.maxPages})
			err := client.WalkDocuments(context.Background(), func([]Document) error { return nil })
			assertPaperlessError(t, err)
		})
	}
}

func TestWalkDocumentsRejectsMalformedPaginationURL(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"count":1,"next":"http://[::1","results":[]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	assertPaperlessError(t, client.WalkDocuments(context.Background(), func([]Document) error { return nil }))
}

func TestWalkDocumentsRejectsPaginationOutsideBasePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		next func(*httptest.Server) string
	}{
		{name: "absolute path", next: func(server *httptest.Server) string { return server.URL + "/api/documents/?page=2" }},
		{name: "relative traversal", next: func(_ *httptest.Server) string { return "../../../api/documents/?page=2" }},
		{name: "encoded traversal", next: func(_ *httptest.Server) string { return "/paperless/%2e%2e/api/documents/?page=2" }},
		{name: "path prefix boundary", next: func(_ *httptest.Server) string { return "/paperless2/api/documents/?page=2" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			var escapedAuth string
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.URL.Path != "/paperless/api/documents/" {
					escapedAuth = request.Header.Get("Authorization")
					io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
					return
				}
				fmt.Fprintf(writer, `{"count":1,"next":%q,"results":[]}`, test.next(server))
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL+"/paperless/", Options{})

			assertPaperlessError(t, client.WalkDocuments(context.Background(), func([]Document) error { return nil }))
			if got, want := requests.Load(), int32(1); got != want {
				t.Errorf("request count = %d, want %d", got, want)
			}
			if escapedAuth != "" {
				t.Errorf("escaped Authorization = %q, want empty", escapedAuth)
			}
		})
	}
}

func TestWalkDocumentsStopsForCallbackAndContext(t *testing.T) {
	t.Parallel()

	callbackErr := errors.New("stop walking")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":1}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})
	if err := client.WalkDocuments(context.Background(), func([]Document) error { return callbackErr }); errors.Is(err, callbackErr) {
		t.Errorf("WalkDocuments() retained callback error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertPaperlessError(t, client.WalkDocuments(ctx, func([]Document) error { return nil }))
}

func TestWalkDocumentsCallbackErrorIsCategorizedAndRedacted(t *testing.T) {
	t.Parallel()

	const callbackCanary = "canary OCR text and secret"
	callbackErr := errors.New(callbackCanary)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":1,"tags":[]}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	err := client.WalkDocuments(context.Background(), func([]Document) error { return callbackErr })
	assertPaperlessError(t, err)
	if errors.Is(err, callbackErr) {
		t.Errorf("errors.Is(error, callback error) = true")
	}
	assertNoPrivateCause(t, err, callbackErr, callbackCanary)
	assertRedacted(t, err, callbackCanary)
}

func TestWalkDocumentsReclassifiesSafeCallbackError(t *testing.T) {
	t.Parallel()

	callbackErr := saferr.New(saferr.CategoryProvider, "canary provider callback")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":1,"tags":[]}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	err := client.WalkDocuments(context.Background(), func([]Document) error { return callbackErr })
	assertPaperlessError(t, err)
	if errors.Is(err, callbackErr) {
		t.Errorf("errors.Is(error, callback error) = true")
	}
	assertNoPrivateCause(t, err, callbackErr, "canary provider callback")
	assertRedacted(t, err, "canary provider callback")
}

func TestWalkDocumentsCallbackContextErrorsPreserveOnlyExactSentinels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":1,"tags":[]}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})
	for _, test := range []struct {
		name     string
		callback error
		wantIs   error
		wantNoIs error
		canary   string
	}{
		{name: "canceled", callback: context.Canceled, wantIs: context.Canceled},
		{name: "deadline", callback: context.DeadlineExceeded, wantIs: context.DeadlineExceeded},
		{name: "wrapped canceled", callback: fmt.Errorf("CANARY wrapped canceled: %w", context.Canceled), wantIs: context.Canceled, canary: "CANARY wrapped canceled"},
		{name: "wrapped deadline", callback: fmt.Errorf("CANARY wrapped deadline: %w", context.DeadlineExceeded), wantIs: context.DeadlineExceeded, canary: "CANARY wrapped deadline"},
		{name: "both prefers canceled", callback: errors.Join(fmt.Errorf("CANARY canceled: %w", context.Canceled), fmt.Errorf("CANARY deadline: %w", context.DeadlineExceeded)), wantIs: context.Canceled, wantNoIs: context.DeadlineExceeded, canary: "CANARY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := client.WalkDocuments(context.Background(), func([]Document) error { return test.callback })
			assertPaperlessError(t, err)
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Errorf("errors.Is(error, %v) = false", test.wantIs)
			}
			if test.wantNoIs != nil && errors.Is(err, test.wantNoIs) {
				t.Errorf("errors.Is(error, %v) = true", test.wantNoIs)
			}
			assertNoPrivateCause(t, err, test.callback, test.canary)
		})
	}
}

func TestGetDocumentAndChecksum(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.Path, "/paperless/api/documents/42/"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		io.WriteString(writer, `{"id":42,"content":"native text","checksum":"source-checksum","tags":[7,8],"ignored":"future field"}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL+"/paperless", Options{})

	document, err := client.GetDocument(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetDocument() error = %v", err)
	}
	if got, want := document, (Document{ID: 42, Content: "native text", Checksum: "source-checksum", Tags: []int{7, 8}}); !documentEqual(got, want) {
		t.Errorf("GetDocument() = %#v, want %#v", got, want)
	}
	checksum, err := client.GetChecksum(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetChecksum() error = %v", err)
	}
	if checksum != "source-checksum" {
		t.Errorf("GetChecksum() = %q, want source-checksum", checksum)
	}
}

func TestGetDocumentRejectsInvalidDecodedRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid document ID", body: `{"id":0,"content":"canary response content","tags":[]}`},
		{name: "invalid document tag ID", body: `{"id":1,"content":"canary response content","tags":[0]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				io.WriteString(writer, test.body)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, Options{})

			_, err := client.GetDocument(context.Background(), 1)
			assertPaperlessError(t, err)
			assertRedacted(t, err, "canary response content")
		})
	}
}

func TestWalkDocumentsRejectsInvalidRecordBeforeVisitor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid document ID", body: `{"count":1,"next":null,"results":[{"id":-7,"tags":[]}]}`},
		{name: "invalid document tag ID", body: `{"count":1,"next":null,"results":[{"id":1,"tags":[-9]}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				io.WriteString(writer, test.body)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, Options{})
			var visited atomic.Bool

			err := client.WalkDocuments(context.Background(), func([]Document) error {
				visited.Store(true)
				return nil
			})
			assertPaperlessError(t, err)
			if visited.Load() {
				t.Error("visitor called with invalid document page")
			}
		})
	}
}

func TestDownloadOriginalStreamsAndLimitsResponse(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte("0123456789"), 7000)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.Path, "/api/documents/9/download/"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got, want := request.URL.Query().Get("original"), "true"; got != want {
			t.Errorf("original = %q, want %q", got, want)
		}
		writer.Write(payload)
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL, Options{MaxDownloadBytes: int64(len(payload))})
	var destination chunkWriter
	if err := client.DownloadOriginal(context.Background(), 9, &destination); err != nil {
		t.Fatalf("DownloadOriginal() error = %v", err)
	}
	if !bytes.Equal(destination.Bytes(), payload) {
		t.Errorf("download = %q, want %q", destination.Bytes(), payload)
	}
	if destination.writes < 2 {
		t.Errorf("writer calls = %d, want incremental writes", destination.writes)
	}

	limited := newTestClient(t, server.URL, Options{MaxDownloadBytes: int64(len(payload) - 1)})
	assertPaperlessError(t, limited.DownloadOriginal(context.Background(), 9, io.Discard))
}

func TestDownloadOriginalRejectsPartialContentWithoutWriting(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusPartialContent)
		io.WriteString(writer, "partial PDF bytes")
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	var destination bytes.Buffer
	err := client.DownloadOriginal(context.Background(), 1, &destination)
	assertPaperlessError(t, err)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("errors.As(*StatusError) = false: %v", err)
	}
	if got, want := statusErr.StatusCode, http.StatusPartialContent; got != want {
		t.Errorf("StatusCode = %d, want %d", got, want)
	}
	if destination.Len() != 0 {
		t.Errorf("destination length = %d, want 0", destination.Len())
	}
}

func TestUpdateContentAndTagsUseSeparatePatches(t *testing.T) {
	t.Parallel()

	const content = "canary OCR content"
	requests := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPatch {
			t.Errorf("method = %q, want PATCH", request.Method)
		}
		if got, want := request.Header.Get("Content-Type"), "application/json"; got != want {
			t.Errorf("Content-Type = %q, want %q", got, want)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		requests <- body
		writer.WriteHeader(http.StatusOK)
		io.WriteString(writer, `{}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	if err := client.UpdateContent(context.Background(), 4, content); err != nil {
		t.Fatalf("UpdateContent() error = %v", err)
	}
	if err := client.UpdateTags(context.Background(), 4, []int{8, 3, 8, 5}, []int{7, 3}, []int{5}); err != nil {
		t.Fatalf("UpdateTags() error = %v", err)
	}

	if got, want := <-requests, map[string]any{"content": content}; !mapsEqual(got, want) {
		t.Errorf("content PATCH = %#v, want %#v", got, want)
	}
	if got, want := <-requests, map[string]any{"tags": []any{float64(3), float64(7), float64(8)}}; !mapsEqual(got, want) {
		t.Errorf("tags PATCH = %#v, want %#v", got, want)
	}
}

func TestReplaceTagsEncodesEmptyArray(t *testing.T) {
	t.Parallel()

	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ = io.ReadAll(request.Body)
		io.WriteString(writer, `{}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	if err := client.ReplaceTags(context.Background(), 4, nil); err != nil {
		t.Fatalf("ReplaceTags() error = %v", err)
	}
	if got, want := string(body), `{"tags":[]}`; got != want {
		t.Errorf("ReplaceTags() body = %q, want %q", got, want)
	}
}

func TestDownloadLimitDoesNotWriteProbeByte(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, "12345")
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{MaxDownloadBytes: 4})

	var destination bytes.Buffer
	assertPaperlessError(t, client.DownloadOriginal(context.Background(), 1, &destination))
	if got, want := destination.String(), "1234"; got != want {
		t.Errorf("downloaded bytes = %q, want %q", got, want)
	}
}

func TestEnsureTagLooksUpExactNameAndCreatesMissingTag(t *testing.T) {
	t.Parallel()

	var listCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.Query().Get("name__iexact"), "OCR complete & reviewed"; request.Method == http.MethodGet && got != want {
			t.Errorf("name__iexact = %q, want %q", got, want)
		}
		switch request.Method {
		case http.MethodGet:
			if listCalls.Add(1) == 1 {
				io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":11,"name":"OCR complete & reviewed"}]}`)
				return
			}
			io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
		case http.MethodPost:
			var body map[string]string
			json.NewDecoder(request.Body).Decode(&body)
			if got, want := body["name"], "OCR complete & reviewed"; got != want {
				t.Errorf("created name = %q, want %q", got, want)
			}
			writer.WriteHeader(http.StatusCreated)
			io.WriteString(writer, `{"id":12,"name":"OCR complete & reviewed"}`)
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	first, err := client.EnsureTag(context.Background(), "OCR complete & reviewed")
	if err != nil || first.ID != 11 {
		t.Fatalf("first EnsureTag() = %#v, %v, want ID 11", first, err)
	}
	second, err := client.EnsureTag(context.Background(), "OCR complete & reviewed")
	if err != nil || second.ID != 12 {
		t.Fatalf("second EnsureTag() = %#v, %v, want ID 12", second, err)
	}
}

func TestEnsureTagRejectsAmbiguousDuplicates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"count":2,"next":null,"results":[{"id":1,"name":"state"},{"id":2,"name":"state"}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})
	assertPaperlessError(t, func() error {
		_, err := client.EnsureTag(context.Background(), "state")
		return err
	}())
}

func TestEnsureTagFindsExactMatchOnLaterPageWithoutCreating(t *testing.T) {
	t.Parallel()

	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			posts.Add(1)
			http.Error(writer, "unexpected create", http.StatusInternalServerError)
			return
		}
		switch request.URL.Query().Get("page") {
		case "":
			if got, want := request.URL.Query().Get("name__iexact"), "state & done"; got != want {
				t.Errorf("name__iexact = %q, want %q", got, want)
			}
			io.WriteString(writer, `{"count":2,"next":"?name__iexact=state+%26+done&page=2","results":[{"id":1,"name":"STATE & DONE"}]}`)
		case "2":
			io.WriteString(writer, `{"count":2,"next":null,"results":[{"id":2,"name":"state & done"}]}`)
		default:
			http.Error(writer, "unexpected page", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	tag, err := client.EnsureTag(context.Background(), "state & done")
	if err != nil {
		t.Fatalf("EnsureTag() error = %v", err)
	}
	if got, want := tag.ID, 2; got != want {
		t.Errorf("EnsureTag() ID = %d, want %d", got, want)
	}
	if posts.Load() != 0 {
		t.Errorf("POST count = %d, want 0", posts.Load())
	}
}

func TestEnsureTagRejectsDuplicatesAcrossPages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			t.Error("EnsureTag() created an ambiguous tag")
			return
		}
		if request.URL.Query().Get("page") == "2" {
			io.WriteString(writer, `{"count":2,"next":null,"results":[{"id":2,"name":"state"}]}`)
			return
		}
		io.WriteString(writer, `{"count":2,"next":"?name__iexact=state&page=2","results":[{"id":1,"name":"state"}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	_, err := client.EnsureTag(context.Background(), "state")
	assertPaperlessError(t, err)
}

func TestEnsureTagRejectsUnsafePagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		maxPages int
		next     func(*httptest.Server) string
	}{
		{name: "loop", next: func(server *httptest.Server) string { return server.URL + "/paperless/api/tags/?name__iexact=state" }},
		{name: "page limit", maxPages: 1, next: func(_ *httptest.Server) string { return "?name__iexact=state&page=2" }},
		{name: "subpath escape", next: func(server *httptest.Server) string { return server.URL + "/api/tags/?name__iexact=state&page=2" }},
		{name: "encoded traversal", next: func(_ *httptest.Server) string { return "/paperless/%2e%2e/api/tags/?name__iexact=state&page=2" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.Method == http.MethodPost {
					t.Error("EnsureTag() created before safe traversal completed")
					return
				}
				fmt.Fprintf(writer, `{"count":0,"next":%q,"results":[]}`, test.next(server))
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL+"/paperless/", Options{MaxPages: test.maxPages})

			_, err := client.EnsureTag(context.Background(), "state")
			assertPaperlessError(t, err)
			if test.name == "subpath escape" || test.name == "encoded traversal" {
				if got, want := requests.Load(), int32(1); got != want {
					t.Errorf("request count = %d, want %d", got, want)
				}
			}
		})
	}
}

func TestEnsureTagRejectsInvalidLookupTag(t *testing.T) {
	t.Parallel()

	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			posts.Add(1)
			return
		}
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":0,"name":"canary tag name"}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	_, err := client.EnsureTag(context.Background(), "canary tag name")
	assertPaperlessError(t, err)
	assertRedacted(t, err, "canary tag name")
	if posts.Load() != 0 {
		t.Errorf("POST count = %d, want 0", posts.Load())
	}
}

func TestEnsureTagRejectsInvalidCreatedTag(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
			return
		}
		writer.WriteHeader(http.StatusCreated)
		io.WriteString(writer, `{"id":-1,"name":"canary created tag"}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	_, err := client.EnsureTag(context.Background(), "canary created tag")
	assertPaperlessError(t, err)
	assertRedacted(t, err, "canary created tag")
}

func TestEnsureTagSerializesConcurrentCreation(t *testing.T) {
	t.Parallel()

	const callers = 20
	var stateMu sync.Mutex
	var exists bool
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			stateMu.Lock()
			found := exists
			stateMu.Unlock()
			time.Sleep(30 * time.Millisecond)
			if found {
				io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":77,"name":"concurrent"}]}`)
				return
			}
			io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
		case http.MethodPost:
			posts.Add(1)
			stateMu.Lock()
			exists = true
			stateMu.Unlock()
			writer.WriteHeader(http.StatusCreated)
			io.WriteString(writer, `{"id":77,"name":"concurrent"}`)
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	start := make(chan struct{})
	results := make(chan Tag, callers)
	errorsChannel := make(chan error, callers)
	var waitGroup sync.WaitGroup
	for range callers {
		waitGroup.Go(func() {
			<-start
			tag, err := client.EnsureTag(context.Background(), "concurrent")
			results <- tag
			errorsChannel <- err
		})
	}
	close(start)
	waitGroup.Wait()
	close(results)
	close(errorsChannel)

	for err := range errorsChannel {
		if err != nil {
			t.Errorf("EnsureTag() error = %v", err)
		}
	}
	for tag := range results {
		if got, want := tag.ID, 77; got != want {
			t.Errorf("EnsureTag() ID = %d, want %d", got, want)
		}
	}
	if got, want := posts.Load(), int32(1); got != want {
		t.Errorf("POST count = %d, want %d", got, want)
	}
}

func TestEnsureTagWaiterHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	firstLookupStarted := make(chan struct{})
	releaseFirstLookup := make(chan struct{})
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			if lookups.Add(1) == 1 {
				close(firstLookupStarted)
				<-releaseFirstLookup
			}
			io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":3,"name":"serialized"}]}`)
			return
		}
		http.Error(writer, "unexpected POST", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	firstResult := make(chan error, 1)
	go func() {
		_, err := client.EnsureTag(context.Background(), "serialized")
		firstResult <- err
	}()
	<-firstLookupStarted

	ctx, cancel := context.WithCancel(context.Background())
	secondResult := make(chan error, 1)
	go func() {
		_, err := client.EnsureTag(ctx, "serialized")
		secondResult <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	secondErr := <-secondResult
	close(releaseFirstLookup)
	firstErr := <-firstResult

	assertPaperlessError(t, secondErr)
	if !errors.Is(secondErr, context.Canceled) {
		t.Errorf("errors.Is(error, context.Canceled) = false")
	}
	if got, want := lookups.Load(), int32(1); got != want {
		t.Errorf("lookup count before release = %d, want %d", got, want)
	}
	if firstErr != nil {
		t.Errorf("first EnsureTag() error = %v", firstErr)
	}
}

func TestEnsureTagRecoversFromCreateConflictByRelookingUp(t *testing.T) {
	t.Parallel()

	var lookups atomic.Int32
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if lookups.Add(1) == 1 {
				io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
				return
			}
			io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":88,"name":"external race"}]}`)
		case http.MethodPost:
			posts.Add(1)
			writer.WriteHeader(http.StatusConflict)
			io.WriteString(writer, `{"detail":"canary external conflict body"}`)
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	tag, err := client.EnsureTag(context.Background(), "external race")
	if err != nil {
		t.Fatalf("EnsureTag() error = %v", err)
	}
	if got, want := tag.ID, 88; got != want {
		t.Errorf("EnsureTag() ID = %d, want %d", got, want)
	}
	if got, want := lookups.Load(), int32(2); got != want {
		t.Errorf("lookup count = %d, want %d", got, want)
	}
	if got, want := posts.Load(), int32(1); got != want {
		t.Errorf("POST count = %d, want %d", got, want)
	}
}

func TestEnsureTagRecoversFromBadRequestByRelookingUp(t *testing.T) {
	t.Parallel()

	const responseCanary = "canary duplicate validation body"
	var lookups atomic.Int32
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			if lookups.Add(1) == 1 {
				io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
				return
			}
			io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":89,"name":"duplicate"}]}`)
		case http.MethodPost:
			posts.Add(1)
			writer.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(writer, `{"name":[%q]}`, responseCanary)
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	tag, err := client.EnsureTag(context.Background(), "duplicate")
	if err != nil {
		t.Fatalf("EnsureTag() error = %v", err)
	}
	if got, want := tag.ID, 89; got != want {
		t.Errorf("EnsureTag() ID = %d, want %d", got, want)
	}
	if got, want := lookups.Load(), int32(2); got != want {
		t.Errorf("lookup count = %d, want %d", got, want)
	}
	if got, want := posts.Load(), int32(1); got != want {
		t.Errorf("POST count = %d, want %d", got, want)
	}
}

func TestIndependentClientsRecoverConcurrentTagCreation(t *testing.T) {
	t.Parallel()

	const tagName = "cross-client race"
	var stateMu sync.Mutex
	var exists bool
	var lookups atomic.Int32
	var posts atomic.Int32
	initialLookupsReady := make(chan struct{})
	var readyOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			lookup := lookups.Add(1)
			if lookup <= 2 {
				if lookup == 2 {
					readyOnce.Do(func() { close(initialLookupsReady) })
				}
				<-initialLookupsReady
				io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
				return
			}
			stateMu.Lock()
			found := exists
			stateMu.Unlock()
			if !found {
				t.Error("relookup occurred before tag creation")
			}
			fmt.Fprintf(writer, `{"count":1,"next":null,"results":[{"id":90,"name":%q}]}`, tagName)
		case http.MethodPost:
			posts.Add(1)
			stateMu.Lock()
			if exists {
				stateMu.Unlock()
				writer.WriteHeader(http.StatusBadRequest)
				io.WriteString(writer, `{"name":["already exists"]}`)
				return
			}
			exists = true
			stateMu.Unlock()
			writer.WriteHeader(http.StatusCreated)
			fmt.Fprintf(writer, `{"id":90,"name":%q}`, tagName)
		default:
			http.Error(writer, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	firstClient := newTestClient(t, server.URL, Options{})
	secondClient := newTestClient(t, server.URL, Options{})

	start := make(chan struct{})
	results := make(chan Tag, 2)
	errorsChannel := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for _, client := range []*Client{firstClient, secondClient} {
		waitGroup.Go(func() {
			<-start
			tag, err := client.EnsureTag(context.Background(), tagName)
			results <- tag
			errorsChannel <- err
		})
	}
	close(start)
	waitGroup.Wait()
	close(results)
	close(errorsChannel)

	for err := range errorsChannel {
		if err != nil {
			t.Errorf("EnsureTag() error = %v", err)
		}
	}
	for tag := range results {
		if got, want := tag.ID, 90; got != want {
			t.Errorf("EnsureTag() ID = %d, want %d", got, want)
		}
	}
	if got, want := posts.Load(), int32(2); got != want {
		t.Errorf("POST count = %d, want %d", got, want)
	}
	if got, want := lookups.Load(), int32(3); got != want {
		t.Errorf("lookup count = %d, want %d", got, want)
	}
}

func TestEnsureTagConflictRelookupMustFindExactlyOneTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result string
	}{
		{name: "absent", result: `{"count":0,"next":null,"results":[]}`},
		{name: "ambiguous", result: `{"count":2,"next":null,"results":[{"id":1,"name":"race"},{"id":2,"name":"race"}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var lookups atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					if lookups.Add(1) == 1 {
						io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
						return
					}
					io.WriteString(writer, test.result)
					return
				}
				writer.WriteHeader(http.StatusBadRequest)
				io.WriteString(writer, `{"detail":"canary original create body"}`)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL, Options{})

			_, err := client.EnsureTag(context.Background(), "race")
			assertPaperlessError(t, err)
			var statusErr *StatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("errors.As(*StatusError) = false: %v", err)
			}
			if got, want := statusErr.StatusCode, http.StatusBadRequest; got != want {
				t.Errorf("StatusCode = %d, want %d", got, want)
			}
			if got, want := lookups.Load(), int32(2); got != want {
				t.Errorf("lookup count = %d, want %d", got, want)
			}
			assertRedacted(t, err, "canary original create body")
		})
	}
}

func TestEnsureTagRelookupFailurePreservesOriginalCreateError(t *testing.T) {
	t.Parallel()

	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			if lookups.Add(1) == 1 {
				io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
				return
			}
			writer.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(writer, `{"detail":"canary relookup body"}`)
			return
		}
		writer.WriteHeader(http.StatusBadRequest)
		io.WriteString(writer, `{"detail":"canary original create body"}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	_, err := client.EnsureTag(context.Background(), "race")
	assertPaperlessError(t, err)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("errors.As(*StatusError) = false: %v", err)
	}
	if got, want := statusErr.StatusCode, http.StatusBadRequest; got != want {
		t.Errorf("StatusCode = %d, want original %d", got, want)
	}
	if got, want := lookups.Load(), int32(2); got != want {
		t.Errorf("lookup count = %d, want %d", got, want)
	}
	assertRedacted(t, err, "canary original create body", "canary relookup body")
}

func TestRequestDeadlineAndJSONLimit(t *testing.T) {
	t.Parallel()

	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			io.WriteString(writer, `{"id":1}`)
		}))
		t.Cleanup(server.Close)
		client := newTestClient(t, server.URL, Options{RequestTimeout: 20 * time.Millisecond})
		_, err := client.GetDocument(context.Background(), 1)
		assertPaperlessError(t, err)
	})

	t.Run("JSON limit", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			io.WriteString(writer, `{"id":1,"content":"a valid prefix followed by excessive data"}`)
		}))
		t.Cleanup(server.Close)
		client := newTestClient(t, server.URL, Options{MaxJSONResponseBytes: 16})
		_, err := client.GetDocument(context.Background(), 1)
		assertPaperlessError(t, err)
	})
}

func TestStatusErrorIsTypedCategorizedAndRedacted(t *testing.T) {
	t.Parallel()

	const responseBody = `{"detail":"canary response body"}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		io.WriteString(writer, responseBody)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	_, err := client.GetDocument(context.Background(), 6)
	assertPaperlessError(t, err)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("errors.As(*StatusError) = false: %v", err)
	}
	if statusErr.StatusCode != http.StatusUnauthorized || statusErr.Operation != "get document" {
		t.Errorf("StatusError = %#v", statusErr)
	}
	assertRedacted(t, err, testToken, responseBody, "canary response body")
}

func TestRedirectDoesNotForwardAuthorizationCrossOrigin(t *testing.T) {
	t.Parallel()

	var receivedAuth string
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedAuth = request.Header.Get("Authorization")
		io.WriteString(writer, `{"id":1}`)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", target.URL)
		writer.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(source.Close)
	client := newTestClient(t, source.URL, Options{})

	_, err := client.GetDocument(context.Background(), 1)
	assertPaperlessError(t, err)
	if receivedAuth != "" {
		t.Errorf("cross-origin Authorization = %q, want empty", receivedAuth)
	}
}

func TestSameOriginRedirectStripsAuthorization(t *testing.T) {
	t.Parallel()

	var redirectedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/documents/1/" {
			http.Redirect(writer, request, "/redirected", http.StatusFound)
			return
		}
		redirectedAuth = request.Header.Get("Authorization")
		io.WriteString(writer, `{"id":1}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})

	if _, err := client.GetDocument(context.Background(), 1); err != nil {
		t.Fatalf("GetDocument() error = %v", err)
	}
	if redirectedAuth != "" {
		t.Errorf("redirected Authorization = %q, want empty", redirectedAuth)
	}
}

func TestMutatingRedirectsAreRejectedBeforeMethodRewrite(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther} {
		for _, operation := range []string{"update content", "ensure tag"} {
			t.Run(fmt.Sprintf("%s_%d", operation, status), func(t *testing.T) {
				t.Parallel()

				var redirectedRequests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/redirected" {
						redirectedRequests.Add(1)
						if request.Method == http.MethodGet {
							t.Error("mutating request was rewritten to GET")
						}
						io.WriteString(writer, `{}`)
						return
					}
					if operation == "ensure tag" && request.Method == http.MethodGet {
						io.WriteString(writer, `{"count":0,"next":null,"results":[]}`)
						return
					}
					writer.Header().Set("Location", "/redirected")
					writer.WriteHeader(status)
				}))
				t.Cleanup(server.Close)
				client := newTestClient(t, server.URL, Options{})

				var err error
				if operation == "update content" {
					err = client.UpdateContent(context.Background(), 1, "canary redirected OCR content")
				} else {
					_, err = client.EnsureTag(context.Background(), "redirected tag")
				}
				assertPaperlessError(t, err)
				assertRedacted(t, err, testToken, "canary redirected OCR content")
				if got := redirectedRequests.Load(); got != 0 {
					t.Errorf("redirected request count = %d, want 0", got)
				}
			})
		}
	}
}

func TestCallerRedirectPolicyIsPreserved(t *testing.T) {
	t.Parallel()

	const callerCanary = "CANARY caller redirect endpoint/header/key"
	callerErr := errors.New(callerCanary)
	var policyCalls atomic.Int32
	var policyMethod string
	var policyRequest *http.Request
	httpClient := &http.Client{CheckRedirect: func(request *http.Request, via []*http.Request) error {
		policyCalls.Add(1)
		policyMethod = via[0].Method
		policyRequest = request
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("caller policy Authorization = %q, want empty", authorization)
		}
		request.Header.Set("Authorization", "Token CANARY-restored-error-secret")
		if request.URL.Path != "/paperless/redirected" || len(via) != 1 {
			t.Errorf("caller policy request = %s, via = %d", request.URL.Path, len(via))
		}
		return callerErr
	}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/paperless/api/documents/1/" {
			http.Redirect(writer, request, "/paperless/redirected", http.StatusFound)
			return
		}
		t.Error("caller-rejected redirect was followed")
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL+"/paperless/", Options{HTTPClient: httpClient})

	_, err := client.GetDocument(context.Background(), 1)
	assertPaperlessError(t, err)
	if errors.Is(err, callerErr) {
		t.Errorf("errors.Is(error, caller error) = true")
	}
	if got, want := policyCalls.Load(), int32(1); got != want {
		t.Errorf("caller policy calls = %d, want %d", got, want)
	}
	if got, want := policyMethod, http.MethodGet; got != want {
		t.Errorf("original method = %q, want %q", got, want)
	}
	if authorization := policyRequest.Header.Get("Authorization"); authorization != "" {
		t.Errorf("rejected redirect Authorization = %q, want empty", authorization)
	}
	assertRedacted(t, err, callerCanary, testToken, "CANARY-restored-error-secret", server.URL, "/paperless/redirected", "Authorization")
}

func TestCallerRedirectPolicyCannotRestoreAuthorization(t *testing.T) {
	var destinationAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/paperless/api/documents/1/" {
			http.Redirect(writer, request, "/paperless/redirected", http.StatusFound)
			return
		}
		destinationAuthorization = request.Header.Get("Authorization")
		io.WriteString(writer, `{"id":1}`)
	}))
	t.Cleanup(server.Close)
	httpClient := &http.Client{CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		if request.Header.Get("Authorization") != "" {
			t.Error("caller inspected inherited Authorization")
		}
		request.Header.Set("Authorization", "Token CANARY-restored-secret")
		return nil
	}}
	client := newTestClient(t, server.URL+"/paperless/", Options{HTTPClient: httpClient})
	if _, err := client.GetDocument(context.Background(), 1); err != nil {
		t.Fatalf("GetDocument() error = %v", err)
	}
	if destinationAuthorization != "" {
		t.Errorf("destination Authorization = %q, want empty", destinationAuthorization)
	}
}

func TestMandatoryRedirectPolicyRunsBeforeCallerPolicy(t *testing.T) {
	t.Parallel()

	var policyCalls atomic.Int32
	httpClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		policyCalls.Add(1)
		return nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Location", "/outside")
		writer.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL+"/paperless/", Options{HTTPClient: httpClient})

	_, err := client.GetDocument(context.Background(), 1)
	assertPaperlessError(t, err)
	if got := policyCalls.Load(); got != 0 {
		t.Errorf("caller policy calls = %d, want 0", got)
	}
}

func TestSameOriginRedirectOutsideBasePathIsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		location string
	}{
		{name: "absolute path", location: "/api/documents/1/"},
		{name: "relative traversal", location: "../../../../api/documents/1/"},
		{name: "encoded traversal", location: "/paperless/%2e%2e/api/documents/1/"},
		{name: "path prefix boundary", location: "/paperless2/api/documents/1/"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			var escapedAuth string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				if request.URL.Path != "/paperless/api/documents/1/" {
					escapedAuth = request.Header.Get("Authorization")
					io.WriteString(writer, `{"id":1}`)
					return
				}
				writer.Header().Set("Location", test.location)
				writer.WriteHeader(http.StatusFound)
			}))
			t.Cleanup(server.Close)
			client := newTestClient(t, server.URL+"/paperless/", Options{})

			_, err := client.GetDocument(context.Background(), 1)
			assertPaperlessError(t, err)
			if got, want := requests.Load(), int32(1); got != want {
				t.Errorf("request count = %d, want %d", got, want)
			}
			if escapedAuth != "" {
				t.Errorf("escaped Authorization = %q, want empty", escapedAuth)
			}
		})
	}
}

func TestConstructorDoesNotMutateInputs(t *testing.T) {
	t.Parallel()

	baseURL, err := url.Parse("https://paperless.example/subpath?ignored=query")
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{}
	if _, err := New(baseURL, testToken, Options{HTTPClient: httpClient}); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got, want := baseURL.String(), "https://paperless.example/subpath?ignored=query"; got != want {
		t.Errorf("base URL = %q, want unchanged %q", got, want)
	}
	if httpClient.CheckRedirect != nil {
		t.Error("HTTP client CheckRedirect was mutated")
	}
}

func TestValidationRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, "https://paperless.example", Options{})
	for _, call := range []func() error{
		func() error { _, err := client.GetDocument(context.Background(), 0); return err },
		func() error { return client.DownloadOriginal(context.Background(), -1, io.Discard) },
		func() error { return client.DownloadOriginal(context.Background(), 1, nil) },
		func() error { return client.ReplaceTags(context.Background(), 1, []int{0}) },
		func() error { _, err := client.EnsureTag(context.Background(), " "); return err },
	} {
		if err := call(); err == nil {
			t.Error("operation error = nil, want validation error")
		}
	}
}

func newTestClient(t *testing.T, rawURL string, options Options) *Client {
	t.Helper()
	baseURL, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	client, err := New(baseURL, testToken, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func assertPaperlessError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want Paperless error")
	}
	var safeErr *saferr.Error
	if !errors.As(err, &safeErr) {
		t.Fatalf("errors.As(*saferr.Error) = false: %v", err)
	}
	if safeErr.Category() != saferr.CategoryPaperless {
		t.Errorf("error category = %q, want %q", safeErr.Category(), saferr.CategoryPaperless)
	}
}

func assertRedacted(t *testing.T, err error, canaries ...string) {
	t.Helper()
	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		formatted := fmt.Sprintf(format, err)
		for _, canary := range canaries {
			if strings.Contains(formatted, canary) {
				t.Errorf("format %s disclosed %q: %q", format, canary, formatted)
			}
		}
	}
}

func assertNoPrivateCause(t *testing.T, err, private error, canaries ...string) {
	t.Helper()
	if private != context.Canceled && private != context.DeadlineExceeded && errors.Is(err, private) {
		t.Errorf("private cause remains traversable: %v", err)
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		for _, formatted := range []string{current.Error(), fmt.Sprintf("%s", current), fmt.Sprintf("%v", current), fmt.Sprintf("%+v", current), fmt.Sprintf("%q", current)} {
			for _, canary := range canaries {
				if canary != "" && strings.Contains(formatted, canary) {
					t.Errorf("private callback data %q in %q", canary, formatted)
				}
			}
		}
	}
}

func documentsEqual(left, right []Document) bool {
	return slices.EqualFunc(left, right, documentEqual)
}

func documentEqual(left, right Document) bool {
	return left.ID == right.ID && left.Content == right.Content && left.Checksum == right.Checksum && slices.Equal(left.Tags, right.Tags)
}

func mapsEqual(left, right map[string]any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

type chunkWriter struct {
	buffer bytes.Buffer
	writes int
}

func (writer *chunkWriter) Write(data []byte) (int, error) {
	writer.writes++
	return writer.buffer.Write(data)
}

func (writer *chunkWriter) Bytes() []byte {
	return writer.buffer.Bytes()
}
