package security_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/aigate"
	"github.com/nosovk/paperless-ai-ocr/internal/observability"
	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/paperlessai"
	"github.com/nosovk/paperless-ai-ocr/internal/queue"
	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
	"github.com/nosovk/paperless-ai-ocr/internal/securelog"
	"github.com/nosovk/paperless-ai-ocr/internal/server"
)

const (
	transportToken = "CANARY-transport-auth-token"
	transportURL   = "https://CANARY-private.example/secret/path"
	transportBody  = "CANARY-private-request-body"
)

const securityDocumentID = int64(918273645)

const (
	inboundTokenCanary        = "CANARY-inbound-bearer-token"
	inboundBodyCanary         = "CANARY-inbound-webhook-body"
	paperlessTokenCanary      = "CANARY-paperless-api-token"
	paperlessURLCanary        = "https://CANARY-paperless.example/private/path"
	paperlessResponseCanary   = "CANARY-paperless-response-body"
	aiKeyCanary               = "CANARY-ai-api-key"
	aiModelCanary             = "CANARY-ai-model-name"
	providerResponseCanary    = "CANARY-provider-response-body"
	providerRefusalCanary     = "CANARY-provider-refusal"
	providerMalformedCanary   = "CANARY-provider-malformed-data"
	paperlessAIKeyCanary      = "CANARY-paperless-ai-key"
	paperlessAIURLCanary      = "https://CANARY-paperless-ai.example/private/webhook"
	paperlessAIResponseCanary = "CANARY-paperless-ai-response-body"
	paperlessAIRequestCanary  = "CANARY-paperless-ai-request-body"
	ocrDraftCanary            = "CANARY-ocr-draft"
	transcriptionCanary       = "CANARY-transcription-output"
	pdfBytesCanary            = "CANARY-pdf-bytes"
	imageBytesCanary          = "CANARY-image-bytes"
	filenameCanary            = "CANARY-secret-filename.pdf"
	pathCanary                = "/CANARY/private/workspace/document.pdf"
	checksumCanary            = "CANARY-source-checksum"
	leaseOwnerCanary          = "CANARY-lease-owner"
)

var forbiddenCanaries = []string{
	inboundTokenCanary, inboundBodyCanary, paperlessTokenCanary, paperlessURLCanary,
	paperlessResponseCanary, aiKeyCanary, aiModelCanary, providerResponseCanary,
	providerRefusalCanary, providerMalformedCanary, paperlessAIKeyCanary,
	paperlessAIURLCanary, paperlessAIResponseCanary, paperlessAIRequestCanary,
	ocrDraftCanary, transcriptionCanary, pdfBytesCanary, imageBytesCanary,
	filenameCanary, pathCanary, checksumCanary, leaseOwnerCanary,
}

func TestSecurityCanarySinkMatrix(t *testing.T) {
	documentID := fmt.Sprintf("%d", securityDocumentID)
	sensitiveError := errors.New(strings.Join(forbiddenCanaries, "|"))

	var logs bytes.Buffer
	logger := securelog.New(&logs)
	if err := logger.JobClaimed(securityDocumentID); err != nil {
		t.Fatal(err)
	}
	if err := logger.JobFinished(securityDocumentID, queue.StateFailed, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := logger.BackgroundFailure(saferr.CategoryInternal); err != nil {
		t.Fatal(err)
	}
	assertSinkContains(t, "structured logs", logs.String(), documentID)
	assertSinkExcludes(t, "structured logs", logs.String(), forbiddenCanaries...)

	webhook, err := server.New(inboundTokenCanary, failingEnqueuer{err: sensitiveError})
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRequest(http.MethodPost, "/webhooks/paperless", strings.NewReader(inboundBodyCanary))
	unauthorized.Header.Set("Authorization", "Bearer wrong-"+inboundTokenCanary)
	unauthorized.Header.Set("Content-Type", "application/json")
	webhookResponse := httptest.NewRecorder()
	webhook.ServeHTTP(webhookResponse, unauthorized)
	assertHTTPResponseSafe(t, "webhook", webhookResponse.Result(), webhookResponse.Body.String(), append(forbiddenCanaries, documentID)...)
	authorized := httptest.NewRequest(http.MethodPost, "/webhooks/paperless", strings.NewReader(`{"document_id":`+documentID+`}`))
	authorized.Header.Set("Authorization", "Bearer "+inboundTokenCanary)
	authorized.Header.Set("Content-Type", "application/json")
	webhookResponse = httptest.NewRecorder()
	webhook.ServeHTTP(webhookResponse, authorized)
	assertHTTPResponseSafe(t, "webhook enqueue failure", webhookResponse.Result(), webhookResponse.Body.String(), append(forbiddenCanaries, documentID)...)

	readiness := server.NewReadiness()
	health := server.NewHealthHandler(readiness)
	for _, path := range []string{"/health", "/ready"} {
		response := httptest.NewRecorder()
		health.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assertHTTPResponseSafe(t, path, response.Result(), response.Body.String(), append(forbiddenCanaries, documentID)...)
	}

	metrics := observability.NewMetrics()
	metrics.SetQueueDepthCollector(func(context.Context) (map[queue.State]int64, error) {
		return nil, sensitiveError
	})
	metricsResponse := httptest.NewRecorder()
	metrics.ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assertHTTPResponseSafe(t, "metrics", metricsResponse.Result(), metricsResponse.Body.String(), append(forbiddenCanaries, documentID)...)

	transport := matrixErrorTransport{canaries: forbiddenCanaries}
	paperlessClient, err := paperless.New(mustURL(t, paperlessURLCanary), paperlessTokenCanary, paperless.Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	providerClient, err := aigate.New(mustURL(t, "https://provider.example/v1"), aiKeyCanary, aiModelCanary, aigate.ClientOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	dispatchClient, err := paperlessai.New(mustURL(t, paperlessAIURLCanary), mustURL(t, paperlessURLCanary), paperlessAIKeyCanary, paperlessai.Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	publicErrors := []error{
		paperlessClient.Ping(context.Background()),
		paperlessDocumentError(t, paperlessClient, int(securityDocumentID)),
		transcriptionError(t, providerClient, aigate.Transcription{Capability: aigate.DirectPDF, FirstPage: 1, LastPage: 1, OCRDraft: ocrDraftCanary, PDF: []byte(pdfBytesCanary)}),
		transcriptionError(t, providerClient, aigate.Transcription{Capability: aigate.PageImages, FirstPage: 1, LastPage: 1, OCRDraft: ocrDraftCanary, Images: [][]byte{[]byte(imageBytesCanary)}}),
		dispatchClient.Dispatch(context.Background(), int(securityDocumentID)),
	}
	for index, publicErr := range publicErrors {
		assertErrorSafe(t, publicErr, append(forbiddenCanaries, documentID)...)
		if publicErr == nil {
			t.Errorf("public error %d = nil", index)
		}
	}
}

func TestHTTPClientTransportErrorsAreSecretSafe(t *testing.T) {
	transport := captureErrorTransport{}
	paperlessURL := mustURL(t, transportURL)
	providerURL := mustURL(t, transportURL)
	webhookURL := mustURL(t, transportURL)
	documentURL := mustURL(t, "https://CANARY-paperless.example/private/")

	paperlessClient, err := paperless.New(paperlessURL, transportToken, paperless.Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	providerClient, err := aigate.New(providerURL, transportToken, "CANARY-private-model", aigate.ClientOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	dispatchClient, err := paperlessai.New(webhookURL, documentURL, transportToken, paperlessai.Options{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}

	calls := []func(context.Context) error{
		paperlessClient.Ping,
		func(ctx context.Context) error { _, err := paperlessClient.GetDocument(ctx, 987654); return err },
		func(ctx context.Context) error { _, err := providerClient.Probe(ctx); return err },
		func(ctx context.Context) error {
			_, err := providerClient.Transcribe(ctx, aigate.Transcription{Capability: aigate.DirectPDF, FirstPage: 1, LastPage: 1, OCRDraft: "CANARY-OCR-draft", PDF: []byte(transportBody)})
			return err
		},
		func(ctx context.Context) error { return dispatchClient.Dispatch(ctx, 987654) },
	}
	for index, call := range calls {
		err := call(context.Background())
		if err == nil {
			t.Fatalf("call %d error = nil", index)
		}
		assertErrorSafe(t, err, transportToken, transportURL, transportBody, "CANARY-private-model", "CANARY-OCR-draft", "987654", "Authorization", "x-api-key")
	}
}

func TestProviderAndPaperlessResponseBodiesAreSecretSafe(t *testing.T) {
	const responseCanary = "CANARY-provider-response-and-OCR-output"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"error":{"type":"invalid_request_error","code":"bad","param":"other","message":"`+responseCanary+`"},"refusal":"`+responseCanary+`"}`)
	}))
	t.Cleanup(server.Close)
	baseURL := mustURL(t, server.URL)

	providerClient, err := aigate.New(baseURL, transportToken, "CANARY-model", aigate.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = providerClient.Transcribe(context.Background(), aigate.Transcription{Capability: aigate.DirectPDF, FirstPage: 1, LastPage: 1, OCRDraft: "CANARY-draft", PDF: []byte("CANARY-PDF-image-bytes")})
	assertErrorSafe(t, err, responseCanary, transportToken, "CANARY-model", "CANARY-draft", "CANARY-PDF-image-bytes", server.URL)

	paperlessClient, err := paperless.New(baseURL, transportToken, paperless.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = paperlessClient.GetDocument(context.Background(), 123)
	assertErrorSafe(t, err, responseCanary, transportToken, server.URL)
}

func TestPaperlessResponseReadErrorsAreSecretSafe(t *testing.T) {
	const canary = "CANARY response reader error with token and path"
	httpClient := &http.Client{Transport: responseErrorTransport{err: errors.New(canary)}}
	client, err := paperless.New(mustURL(t, "https://paperless.example"), transportToken, paperless.Options{HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetDocument(context.Background(), 1)
	assertErrorSafe(t, err, canary, transportToken)
}

type captureErrorTransport struct{}

func (captureErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
	}
	return nil, fmt.Errorf("transport URL=%s Authorization=%s x-api-key=%s body=%s fixed=%s", request.URL, request.Header.Get("Authorization"), request.Header.Get("x-api-key"), body, transportBody)
}

type matrixErrorTransport struct{ canaries []string }

func (transport matrixErrorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
	}
	return nil, fmt.Errorf("%s request_url=%s authorization=%s api_key=%s body=%s",
		strings.Join(transport.canaries, "|"), request.URL, request.Header.Get("Authorization"), request.Header.Get("x-api-key"), body)
}

type failingEnqueuer struct{ err error }

func (enqueuer failingEnqueuer) EnqueueCandidate(context.Context, int64, queue.Priority) (bool, error) {
	return false, enqueuer.err
}

type responseErrorTransport struct{ err error }

func (transport responseErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: errorBody{err: transport.err}}, nil
}

type errorBody struct{ err error }

func (body errorBody) Read([]byte) (int, error) { return 0, body.err }
func (errorBody) Close() error                  { return nil }

func assertErrorSafe(t *testing.T, err error, canaries ...string) {
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

func transcriptionError(t *testing.T, client *aigate.Client, input aigate.Transcription) error {
	t.Helper()
	_, err := client.Transcribe(context.Background(), input)
	return err
}

func paperlessDocumentError(t *testing.T, client *paperless.Client, documentID int) error {
	t.Helper()
	_, err := client.GetDocument(context.Background(), documentID)
	return err
}

func assertHTTPResponseSafe(t *testing.T, name string, response *http.Response, body string, canaries ...string) {
	t.Helper()
	var sink strings.Builder
	sink.WriteString(body)
	for key, values := range response.Header {
		sink.WriteString(key)
		sink.WriteString(strings.Join(values, ","))
	}
	assertSinkExcludes(t, name, sink.String(), canaries...)
}

func assertSinkContains(t *testing.T, name, sink, value string) {
	t.Helper()
	if !strings.Contains(sink, value) {
		t.Errorf("%s missing allowed value %q: %q", name, value, sink)
	}
}

func assertSinkExcludes(t *testing.T, name, sink string, canaries ...string) {
	t.Helper()
	for _, canary := range canaries {
		if strings.Contains(sink, canary) {
			t.Errorf("%s disclosed %q: %q", name, canary, sink)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
