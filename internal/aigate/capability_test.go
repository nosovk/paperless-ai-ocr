package aigate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

func TestProbeFixtures(t *testing.T) {
	if !bytes.HasPrefix(probePDF, []byte("%PDF-")) {
		t.Error("PDF fixture does not have a PDF signature")
	}
	if !bytes.HasPrefix(probePNG, []byte("\x89PNG\r\n\x1a\n")) {
		t.Error("PNG fixture does not have a PNG signature")
	}
	if strings.Contains(probePrompt, probeNonce) {
		t.Error("probe prompt contains visual nonce")
	}
	if strings.Contains(probeFilename, probeNonce) {
		t.Error("probe filename contains visual nonce")
	}
	if bytes.Contains(probePDF, []byte(probeNonce)) || bytes.Contains(probePNG, []byte(probeNonce)) {
		t.Error("visual nonce is stored as extractable fixture text")
	}
}

func TestProbeNilReceiverReturnsSafeProviderError(t *testing.T) {
	const sensitiveCanary = "canary-nil-client-secret"
	var client *Client
	for name, ctx := range map[string]context.Context{
		"background context": context.Background(),
		"nil context":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Probe(ctx)
			if providerCategory(err) != saferr.CategoryProvider {
				t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
			}
			for current := err; current != nil; current = errors.Unwrap(current) {
				formatted := fmt.Sprintf("%s|%v|%+v|%#v", current, current, current, current)
				if strings.Contains(formatted, sensitiveCanary) {
					t.Errorf("error chain disclosed sensitive canary: %q", formatted)
				}
			}
		})
	}
}

func TestNewValidatesAndNormalizesConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		baseURL *url.URL
		apiKey  string
		model   string
		options ClientOptions
	}{
		{name: "nil URL", apiKey: "key", model: "model"},
		{name: "unsupported scheme", baseURL: mustURL(t, "file:///tmp/api"), apiKey: "key", model: "model"},
		{name: "missing host", baseURL: mustURL(t, "https:///v1"), apiKey: "key", model: "model"},
		{name: "userinfo", baseURL: mustURL(t, "https://user@example.com/v1"), apiKey: "key", model: "model"},
		{name: "fragment", baseURL: mustURL(t, "https://example.com/v1#secret"), apiKey: "key", model: "model"},
		{name: "blank API key", baseURL: mustURL(t, "https://example.com/v1"), apiKey: " ", model: "model"},
		{name: "blank model", baseURL: mustURL(t, "https://example.com/v1"), apiKey: "key", model: "\t"},
		{name: "negative timeout", baseURL: mustURL(t, "https://example.com/v1"), apiKey: "key", model: "model", options: ClientOptions{RequestTimeout: -1}},
		{name: "negative response limit", baseURL: mustURL(t, "https://example.com/v1"), apiKey: "key", model: "model", options: ClientOptions{MaxResponseBytes: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.baseURL, test.apiKey, test.model, test.options); providerCategory(err) != saferr.CategoryProvider {
				t.Fatalf("New() error category = %q, want %q", providerCategory(err), saferr.CategoryProvider)
			}
		})
	}

	baseURL := mustURL(t, "https://example.com/gateway/v1/?ignored=yes")
	caller := &http.Client{}
	client, err := New(baseURL, " key ", " model ", ClientOptions{HTTPClient: caller})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got, want := client.responsesURL.String(), "https://example.com/gateway/v1/responses"; got != want {
		t.Errorf("responses URL = %q, want %q", got, want)
	}
	if client.apiKey != " key " || client.model != " model " {
		t.Error("New() altered nonblank credentials or model")
	}
	if client.httpClient == caller {
		t.Error("New() did not clone caller HTTP client")
	}
	if caller.CheckRedirect != nil {
		t.Error("New() mutated caller HTTP client")
	}
	if client.requestTimeout <= 0 || client.maxResponseBytes <= 0 || client.httpClient.Transport == nil {
		t.Error("New() did not apply bounded defaults")
	}
}

func TestProbeDirectPDFRequestAndResponse(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/gateway/v1/responses" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s, want POST /gateway/v1/responses", request.Method, request.URL.Path)
		}
		if got, want := request.Header.Get("Authorization"), "Bearer canary-api-key"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		var body responseRequest
		decodeRequest(t, request.Body, &body)
		assertProbeRequest(t, body, "canary-model", "input_file", probeFilename, "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(probePDF))
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/gateway/v1", "canary-api-key", "canary-model", ClientOptions{})
	capability, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if capability != DirectPDF {
		t.Errorf("Probe() = %q, want %q", capability, DirectPDF)
	}
	if requests.Load() != 1 {
		t.Errorf("request count = %d, want 1", requests.Load())
	}
}

func TestProbeFallsBackOnlyForStructuredUnsupportedPDF(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestNumber := requests.Add(1)
		var body responseRequest
		decodeRequest(t, request.Body, &body)
		if requestNumber == 1 {
			if body.Input[0].Content[1].Type != "input_file" {
				t.Errorf("first attachment type = %q, want input_file", body.Input[0].Content[1].Type)
			}
			writer.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(writer, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"input[0].content[1].file_data","message":"canary-provider-message"}}`)
			return
		}
		assertProbeRequest(t, body, "model", "input_image", "", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(probePNG))
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	capability, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if capability != PageImages {
		t.Errorf("Probe() = %q, want %q", capability, PageImages)
	}
	if requests.Load() != 2 {
		t.Errorf("request count = %d, want 2", requests.Load())
	}
}

func TestProbeDoesNotFallbackForOtherFailures(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "authentication", statusCode: http.StatusUnauthorized, body: `{"error":{"type":"authentication_error","code":"invalid_api_key","param":null}}`},
		{name: "rate limit", statusCode: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit_error","code":"rate_limit","param":null}}`},
		{name: "server", statusCode: http.StatusBadGateway, body: `{"error":{"type":"server_error","code":"unavailable","param":null}}`},
		{name: "malformed", statusCode: http.StatusBadRequest, body: `{`},
		{name: "unknown code", statusCode: http.StatusBadRequest, body: `{"error":{"type":"invalid_request_error","code":"mystery","param":"input[0].content[1].file_data"}}`},
		{name: "unrelated param", statusCode: http.StatusBadRequest, body: `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"temperature"}}`},
		{name: "param containing file substring", statusCode: http.StatusBadRequest, body: `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"profile"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writer.WriteHeader(test.statusCode)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
			if _, err := client.Probe(context.Background()); providerCategory(err) != saferr.CategoryProvider {
				t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
			}
			if requests.Load() != 1 {
				t.Errorf("request count = %d, want 1", requests.Load())
			}
		})
	}
}

func TestProbeRejectsInvalidSuccessResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{`},
		{name: "trailing", body: probeSuccessBody() + `{}`},
		{name: "missing output", body: `{"output":[]}`},
		{name: "wrong output", body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"wrong"}]}]}`},
		{name: "duplicate output", body: fmt.Sprintf(`{"output":[{"type":"message","content":[{"type":"output_text","text":%q},{"type":"output_text","text":%q}]}]}`, probeNonce, probeNonce)},
		{name: "extra empty message", body: fmt.Sprintf(`{"output":[{"type":"message","content":[{"type":"output_text","text":%q}]},{"type":"message","content":[]}]}`, probeNonce)},
		{name: "refusal", body: `{"output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`},
		{name: "output text with refusal", body: fmt.Sprintf(`{"output":[{"type":"message","content":[{"type":"output_text","text":%q,"refusal":"no"}]}]}`, probeNonce)},
		{name: "ambiguous", body: fmt.Sprintf(`{"output":[{"type":"message","content":[{"type":"output_text","text":%q}]},{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`, probeNonce)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
			if _, err := client.Probe(context.Background()); providerCategory(err) != saferr.CategoryProvider {
				t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
			}
		})
	}
}

func TestProbeBoundsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, strings.Repeat("x", 129))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{MaxResponseBytes: 128})
	if _, err := client.Probe(context.Background()); providerCategory(err) != saferr.CategoryProvider {
		t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
	}
}

func TestProbeTerminalUnsupportedIsCached(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestNumber := requests.Add(1)
		var body responseRequest
		decodeRequest(t, request.Body, &body)
		param := "input[0].content[1].file_data"
		if requestNumber == 2 {
			param = "input[0].content[1].image_url"
		}
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(writer, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":%q}}`, param)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	for range 2 {
		if _, err := client.Probe(context.Background()); providerCategory(err) != saferr.CategoryProvider {
			t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
		}
	}
	if requests.Load() != 2 {
		t.Errorf("request count = %d, want 2", requests.Load())
	}
}

func TestProbeDoesNotTreatImageParamAsUnsupportedPDF(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"input[0].content[1].image_url"}}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	if _, err := client.Probe(context.Background()); err == nil {
		t.Fatal("Probe() error = nil, want rejected PDF response")
	}
	if requests.Load() != 1 {
		t.Errorf("request count = %d, want 1", requests.Load())
	}
}

func TestProbeUnrelatedFileSuffixPathsDoNotFallbackOrCache(t *testing.T) {
	params := []string{
		"metadata.file_data",
		"input[0].metadata.file_id",
		"settings.document",
		"request.document_data",
	}
	for _, param := range params {
		t.Run(param, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(writer, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":%q}}`, param)
			}))
			defer server.Close()

			client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
			for range 2 {
				if _, err := client.Probe(context.Background()); providerCategory(err) != saferr.CategoryProvider {
					t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
				}
			}
			if requests.Load() != 2 {
				t.Errorf("request count = %d, want 2", requests.Load())
			}
		})
	}
}

func TestProbeUnrelatedImageSuffixPathsDoNotBecomeCachedTerminal(t *testing.T) {
	params := []string{
		"settings.image_url",
		"metadata.image_data",
		"input[0].metadata.image",
	}
	for _, param := range params {
		t.Run(param, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				var body responseRequest
				decodeRequest(t, request.Body, &body)
				responseParam := "input[0].content[1].file_data"
				if body.Input[0].Content[1].Type == "input_image" {
					responseParam = param
				}
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(writer, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":%q}}`, responseParam)
			}))
			defer server.Close()

			client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
			for range 2 {
				if _, err := client.Probe(context.Background()); providerCategory(err) != saferr.CategoryProvider {
					t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
				}
			}
			if requests.Load() != 4 {
				t.Errorf("request count = %d, want 4", requests.Load())
			}
		})
	}
}

func TestProbePageImagesIsCached(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(writer, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"input[0].content[1].file_data"}}`)
			return
		}
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	for range 2 {
		if capability, err := client.Probe(context.Background()); err != nil || capability != PageImages {
			t.Fatalf("Probe() = %q, %v, want %q, nil", capability, err, PageImages)
		}
	}
	if requests.Load() != 2 {
		t.Errorf("request count = %d, want 2", requests.Load())
	}
}

func TestProbeMalformedResponseRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(writer, `{`)
			return
		}
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	if _, err := client.Probe(context.Background()); err == nil {
		t.Fatal("first Probe() error = nil, want malformed response")
	}
	if capability, err := client.Probe(context.Background()); err != nil || capability != DirectPDF {
		t.Fatalf("second Probe() = %q, %v, want %q, nil", capability, err, DirectPDF)
	}
}

func TestProbeSuccessIsCachedAndConcurrentCallersShareLeader(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})

	const callers = 8
	results := make(chan Capability, callers)
	errors := make(chan error, callers)
	for range callers {
		go func() {
			capability, err := client.Probe(context.Background())
			results <- capability
			errors <- err
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range callers {
		if err := <-errors; err != nil {
			t.Errorf("Probe() error = %v", err)
		}
		if capability := <-results; capability != DirectPDF {
			t.Errorf("Probe() = %q, want %q", capability, DirectPDF)
		}
	}
	if requests.Load() != 1 {
		t.Errorf("request count = %d, want 1", requests.Load())
	}
	if _, err := client.Probe(context.Background()); err != nil {
		t.Fatalf("cached Probe() error = %v", err)
	}
	if requests.Load() != 1 {
		t.Errorf("cached request count = %d, want 1", requests.Load())
	}
}

func TestProbeTransientFailureRetries(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	if _, err := client.Probe(context.Background()); err == nil {
		t.Fatal("first Probe() error = nil, want transient failure")
	}
	if capability, err := client.Probe(context.Background()); err != nil || capability != DirectPDF {
		t.Fatalf("second Probe() = %q, %v, want %q, nil", capability, err, DirectPDF)
	}
}

func TestWaiterCancellationDoesNotCancelLeader(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	leaderResult := make(chan error, 1)
	go func() {
		_, err := client.Probe(context.Background())
		leaderResult <- err
	}()
	<-started
	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := client.Probe(waiterCtx)
		waiterResult <- err
	}()
	cancel()
	if err := <-waiterResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-leaderResult; err != nil {
		t.Fatalf("leader error = %v", err)
	}
}

func TestLeaderCancellationPermitsRetry(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = io.Copy(io.Discard, request.Body)
			close(started)
			<-request.Context().Done()
			return
		}
		writeProbeSuccess(t, writer)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{})
	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderResult := make(chan error, 1)
	go func() {
		_, err := client.Probe(leaderCtx)
		leaderResult <- err
	}()
	<-started
	cancel()
	if err := <-leaderResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	if capability, err := client.Probe(context.Background()); err != nil || capability != DirectPDF {
		t.Fatalf("retry Probe() = %q, %v, want %q, nil", capability, err, DirectPDF)
	}
}

func TestProbeTimeoutAndNilContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1", "key", "model", ClientOptions{RequestTimeout: 10 * time.Millisecond})
	if _, err := client.Probe(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe() error = %v, want context.DeadlineExceeded", err)
	}
	if _, err := client.Probe(nil); providerCategory(err) != saferr.CategoryProvider {
		t.Fatalf("Probe(nil) category = %q, want provider", providerCategory(err))
	}
}

func TestProbeErrorsDoNotDiscloseSensitiveDataThroughUnwrapChain(t *testing.T) {
	const (
		apiKey          = "canary-api-key-secret"
		providerMessage = "canary-provider-message-secret"
		providerBody    = "canary-provider-body-secret"
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(writer, `{"error":{"type":"invalid_request_error","code":"bad","param":"temperature","message":%q},"body":%q}`, providerMessage, providerBody)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/v1/canary-url-secret", apiKey, "canary-model-secret", ClientOptions{})
	_, err := client.Probe(context.Background())
	if providerCategory(err) != saferr.CategoryProvider {
		t.Fatalf("Probe() category = %q, want provider", providerCategory(err))
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		formatted := fmt.Sprintf("%s|%v|%+v|%#v", current, current, current, current)
		for _, secret := range []string{apiKey, providerMessage, providerBody, "canary-url-secret", "canary-model-secret", probeNonce, base64.StdEncoding.EncodeToString(probePDF), base64.StdEncoding.EncodeToString(probePNG), probePrompt} {
			if strings.Contains(formatted, secret) {
				t.Errorf("error chain disclosed %q in %q", secret, formatted)
			}
		}
	}
}

func TestRedirectPolicy(t *testing.T) {
	var redirectedAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/base/v1/responses":
			http.Redirect(writer, request, "/base/v1/redirected", http.StatusTemporaryRedirect)
		case "/base/v1/redirected":
			redirectedAuthorization = request.Header.Get("Authorization")
			writeProbeSuccess(t, writer)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/base/v1", "key", "model", ClientOptions{})
	if _, err := client.Probe(context.Background()); err != nil {
		t.Fatalf("same-base redirect Probe() error = %v", err)
	}
	if redirectedAuthorization != "" {
		t.Errorf("redirected Authorization = %q, want empty", redirectedAuthorization)
	}

	externalAuth := make(chan string, 1)
	external := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		externalAuth <- request.Header.Get("Authorization")
		writeProbeSuccess(t, writer)
	}))
	defer external.Close()
	escape := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, external.URL+"/responses", http.StatusTemporaryRedirect)
	}))
	defer escape.Close()
	escapeClient := newTestClient(t, escape.URL+"/base/v1", "key", "model", ClientOptions{})
	if _, err := escapeClient.Probe(context.Background()); err == nil {
		t.Fatal("cross-origin redirect Probe() error = nil")
	}
	select {
	case auth := <-externalAuth:
		t.Errorf("cross-origin redirect reached external server with Authorization %q", auth)
	default:
	}

	pathEscape := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/base/v1/responses" {
			http.Redirect(writer, request, "/outside/responses", http.StatusTemporaryRedirect)
			return
		}
		t.Errorf("base-path escape reached %q", request.URL.Path)
	}))
	defer pathEscape.Close()
	pathEscapeClient := newTestClient(t, pathEscape.URL+"/base/v1", "key", "model", ClientOptions{})
	if _, err := pathEscapeClient.Probe(context.Background()); err == nil {
		t.Fatal("base-path redirect Probe() error = nil")
	}
}

func TestRedirectPolicyStripsAuthorizationBeforeCallerPolicy(t *testing.T) {
	const (
		keyCanary      = "CANARY-ai-key"
		endpointCanary = "/base/v1/CANARY-redirect-endpoint"
		callerCanary   = "CANARY-caller-policy-error"
	)
	callerErr := errors.New(callerCanary)
	var calls int
	httpClient := &http.Client{CheckRedirect: func(request *http.Request, _ []*http.Request) error {
		calls++
		if authorization := request.Header.Get("Authorization"); authorization != "" {
			t.Errorf("caller policy Authorization = %q, want empty", authorization)
		}
		return callerErr
	}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/base/v1/responses" {
			http.Redirect(writer, request, endpointCanary, http.StatusTemporaryRedirect)
			return
		}
		t.Errorf("caller-rejected redirect reached %q", request.URL.Path)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/base/v1", keyCanary, "model", ClientOptions{HTTPClient: httpClient})
	_, err := client.Probe(context.Background())
	if calls != 1 {
		t.Fatalf("caller policy calls = %d, want 1", calls)
	}
	assertSafeErrorChain(t, err, keyCanary, endpointCanary, callerCanary, server.URL, "Authorization")
}

type responseRequest struct {
	Model           string         `json:"model"`
	Input           []responseItem `json:"input"`
	MaxOutputTokens int            `json:"max_output_tokens"`
}

type responseItem struct {
	Role    string            `json:"role"`
	Content []responseContent `json:"content"`
}

type responseContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail"`
}

func assertProbeRequest(t *testing.T, body responseRequest, model, attachmentType, filename, dataURL string) {
	t.Helper()
	if body.Model != model || body.MaxOutputTokens != 32 {
		t.Errorf("model/tokens = %q/%d, want %q/32", body.Model, body.MaxOutputTokens, model)
	}
	if len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) != 2 {
		t.Fatalf("input = %#v, want one user item with prompt and attachment", body.Input)
	}
	prompt, attachment := body.Input[0].Content[0], body.Input[0].Content[1]
	if prompt.Type != "input_text" || prompt.Text != probePrompt || strings.Contains(prompt.Text, probeNonce) {
		t.Errorf("prompt = %#v, want fixed nonce-free input_text", prompt)
	}
	if attachment.Type != attachmentType || attachment.Detail != "low" || attachment.Filename != filename {
		t.Errorf("attachment metadata = %#v", attachment)
	}
	if attachmentType == "input_file" && attachment.FileData != dataURL {
		t.Error("PDF data URL did not match embedded fixture")
	}
	if attachmentType == "input_image" && attachment.ImageURL != dataURL {
		t.Error("image data URL did not match embedded fixture")
	}
}

func decodeRequest(t *testing.T, reader io.Reader, destination any) {
	t.Helper()
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode request: %v", err)
	}
}

func writeProbeSuccess(t *testing.T, writer http.ResponseWriter) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(writer, probeSuccessBody()); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func probeSuccessBody() string {
	return fmt.Sprintf(`{"output":[{"type":"message","content":[{"type":"output_text","text":%q}]}]}`, probeNonce)
}

func newTestClient(t *testing.T, rawURL, apiKey, model string, options ClientOptions) *Client {
	t.Helper()
	client, err := New(mustURL(t, rawURL), apiKey, model, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func mustURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	return parsed
}

func providerCategory(err error) saferr.Category {
	safeError, ok := errors.AsType[*saferr.Error](err)
	if !ok {
		return ""
	}
	return safeError.Category()
}

func assertSafeErrorChain(t *testing.T, err error, canaries ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil")
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		for _, formatted := range []string{current.Error(), fmt.Sprintf("%s", current), fmt.Sprintf("%v", current), fmt.Sprintf("%+v", current), fmt.Sprintf("%q", current)} {
			for _, canary := range canaries {
				if strings.Contains(formatted, canary) {
					t.Errorf("error disclosed %q in %q", canary, formatted)
				}
			}
		}
	}
}

func FuzzCapabilityProviderResponse(f *testing.F) {
	type oracle struct {
		success     bool
		unsupported bool
	}
	seeds := map[string]oracle{}
	addSeed := func(body string, statusCode int, directPDF bool, want oracle) {
		f.Add(body, statusCode, directPDF)
		seeds[fmt.Sprintf("%d\x00%t\x00%s", statusCode, directPDF, body)] = want
	}
	for _, seed := range []struct {
		body       string
		statusCode int
		directPDF  bool
		want       oracle
	}{
		{body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"` + probeNonce + `"}]}]}`, statusCode: http.StatusOK, directPDF: true, want: oracle{success: true}},
		{body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"` + probeNonce + `"}]}]}`, statusCode: http.StatusOK, want: oracle{success: true}},
		{body: `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"input[0].content[1].file_data","message":"FUZZ-CAPABILITY-SECRET"}}`, statusCode: http.StatusBadRequest, directPDF: true, want: oracle{unsupported: true}},
		{body: `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"input[0].content[1].image_url","message":"FUZZ-CAPABILITY-SECRET"}}`, statusCode: http.StatusUnprocessableEntity, want: oracle{unsupported: true}},
		{body: `{"output":[],"output":[{"type":"message","content":[{"type":"output_text","text":"` + probeNonce + `"}]}]}`, statusCode: http.StatusOK, directPDF: true},
		{body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"` + probeNonce + `"}]}],"unknown":"FUZZ-UNKNOWN-SECRET"}`, statusCode: http.StatusOK},
		{body: `{"output":[]} {"trailing":"FUZZ-TRAILING-SECRET"}`, statusCode: http.StatusOK},
		{body: "{\xffFUZZ-MALFORMED-SECRET}", statusCode: http.StatusBadRequest},
		{body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"FUZZ-UNICODE-\ud800"}]}]}`, statusCode: http.StatusOK},
		{body: `{"padding":"FUZZ-OVERSIZE-SECRET-` + strings.Repeat("x", 60<<10) + `"}`, statusCode: http.StatusOK},
	} {
		addSeed(seed.body, seed.statusCode, seed.directPDF, seed.want)
	}
	f.Fuzz(func(t *testing.T, responseBody string, statusCode int, directPDF bool) {
		if len(responseBody) > 1<<16 {
			responseBody = responseBody[:1<<16]
		}
		if statusCode < 100 || statusCode > 599 {
			statusCode = http.StatusBadRequest
		}
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("X-Provider-Private", "FUZZ-CAPABILITY-HEADER-SECRET")
			writer.WriteHeader(statusCode)
			_, _ = io.WriteString(writer, responseBody)
		}))
		defer server.Close()
		client := newTestClient(t, server.URL+"/FUZZ-CAPABILITY-URL-SECRET/v1", "FUZZ-CAPABILITY-KEY-SECRET", "FUZZ-CAPABILITY-MODEL-SECRET", ClientOptions{})
		capability := PageImages
		if directPDF {
			capability = DirectPDF
		}
		unsupported, err := client.probeAttachment(context.Background(), capability)
		if unsupported && err != nil {
			t.Fatalf("probeAttachment() returned unsupported with error: %v", err)
		}
		if err != nil {
			if providerCategory(err) != saferr.CategoryProvider && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("unexpected error class: %T %v", err, err)
			}
			assertFuzzErrorSafe(t, err, responseBody, "FUZZ-CAPABILITY-HEADER-SECRET", "FUZZ-CAPABILITY-URL-SECRET", "FUZZ-CAPABILITY-KEY-SECRET", "FUZZ-CAPABILITY-MODEL-SECRET")
		}
		if want, found := seeds[fmt.Sprintf("%d\x00%t\x00%s", statusCode, directPDF, responseBody)]; found {
			if want.success {
				if unsupported || err != nil {
					t.Fatalf("supported seed = (%t, %v), want false, nil", unsupported, err)
				}
				return
			}
			if unsupported != want.unsupported {
				t.Fatalf("seed unsupported = %t, want %t", unsupported, want.unsupported)
			}
			if !want.unsupported && err == nil {
				t.Fatal("invalid seed error = nil")
			}
		}
	})
}

func TestDecodeSingleJSONAcceptsValidReplacementCharacter(t *testing.T) {
	var response struct {
		Value string `json:"value"`
	}
	data := append([]byte(`{"value":"`), 0xef, 0xbf, 0xbd)
	data = append(data, []byte(`"}`)...)
	if err := decodeSingleJSON(data, &response); err != nil {
		t.Fatalf("decodeSingleJSON() error = %v", err)
	}
	if response.Value != string(rune(0xfffd)) {
		t.Fatalf("value = %q, want replacement character", response.Value)
	}
}
