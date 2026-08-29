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

func TestWalkDocumentsStopsForCallbackAndContext(t *testing.T) {
	t.Parallel()

	callbackErr := errors.New("stop walking")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"count":1,"next":null,"results":[{"id":1}]}`)
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, Options{})
	if err := client.WalkDocuments(context.Background(), func([]Document) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Errorf("WalkDocuments() error = %v, want callback error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertPaperlessError(t, client.WalkDocuments(ctx, func([]Document) error { return nil }))
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

func TestSameOriginRedirectRetainsAuthorization(t *testing.T) {
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
	if got, want := redirectedAuth, "Token "+testToken; got != want {
		t.Errorf("redirected Authorization = %q, want %q", got, want)
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
