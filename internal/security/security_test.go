package security_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nosovk/paperless-ai-ocr/internal/aigate"
	"github.com/nosovk/paperless-ai-ocr/internal/paperless"
	"github.com/nosovk/paperless-ai-ocr/internal/paperlessai"
)

const (
	transportToken = "CANARY-transport-auth-token"
	transportURL   = "https://CANARY-private.example/secret/path"
	transportBody  = "CANARY-private-request-body"
)

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

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
