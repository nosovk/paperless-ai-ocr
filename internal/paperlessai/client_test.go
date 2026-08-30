package paperlessai

import (
	"context"
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

const testWebhookKey = "canary-paperless-ai-key"

func TestDispatchPostsDocumentURLWithDedicatedAuth(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.Method, http.MethodPost; got != want {
			t.Errorf("method = %q, want %q", got, want)
		}
		if got, want := request.Header.Get("x-api-key"), testWebhookKey; got != want {
			t.Errorf("x-api-key = %q, want %q", got, want)
		}
		if got, want := request.Header.Get("Content-Type"), "application/json"; got != want {
			t.Errorf("Content-Type = %q, want %q", got, want)
		}
		data, _ := io.ReadAll(request.Body)
		body = string(data)
		writer.WriteHeader(http.StatusAccepted)
		io.WriteString(writer, `{"status":"queued"}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/api/webhook/document", "https://paperless.example/archive/")

	if err := client.Dispatch(context.Background(), 42); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if got, want := body, `{"url":"https://paperless.example/archive/documents/42/"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestDispatchAcceptsOnlyStatusAcceptedAndRedacts(t *testing.T) {
	const responseCanary = "canary response body"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		io.WriteString(writer, responseCanary)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/secret-endpoint", "https://paperless.example/")

	err := client.Dispatch(context.Background(), 987654)
	assertProviderError(t, err)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusOK {
		t.Fatalf("Dispatch() error = %v, want typed HTTP 200 status", err)
	}
	assertRedacted(t, err, responseCanary, testWebhookKey, server.URL, "secret-endpoint", "987654", "paperless.example")
}

func TestDispatchRejectsRedirectWithoutForwardingKey(t *testing.T) {
	var receivedKey string
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedKey = request.Header.Get("x-api-key")
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer source.Close()
	client := newTestClient(t, source.URL, "https://paperless.example/")

	assertProviderError(t, client.Dispatch(context.Background(), 1))
	if receivedKey != "" {
		t.Errorf("redirected x-api-key = %q, want empty", receivedKey)
	}
}

func TestDispatchHonorsTimeoutAndBoundsResponseDrain(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			writer.WriteHeader(http.StatusAccepted)
		}))
		defer server.Close()
		client := newTestClientWithOptions(t, server.URL, "https://paperless.example/", Options{RequestTimeout: 20 * time.Millisecond})
		assertProviderError(t, client.Dispatch(context.Background(), 1))
	})

	t.Run("bounded drain", func(t *testing.T) {
		body := &countingBody{remaining: 1 << 20}
		httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: body, Header: make(http.Header)}, nil
		})}
		client := newTestClientWithOptions(t, "https://webhook.example/api", "https://paperless.example/", Options{HTTPClient: httpClient, MaxResponseDrainBytes: 32})
		assertProviderError(t, client.Dispatch(context.Background(), 1))
		if got, want := body.read.Load(), int64(32); got != want {
			t.Errorf("response bytes read = %d, want %d", got, want)
		}
	})
}

func TestDispatchTransportErrorDoesNotExposePrivateCause(t *testing.T) {
	const (
		endpointCanary = "https://webhook.example/secret-endpoint"
		documentCanary = "https://paperless.example/private/documents/987654/"
		bodyCanary     = "canary request body"
		credential     = "canary custom credential"
	)
	privateCause := fmt.Errorf("transport failed endpoint=%s document=%s body=%s credential=%s", endpointCanary, documentCanary, bodyCanary, credential)
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, privateCause
	})}
	client := newTestClientWithOptions(t, endpointCanary, "https://paperless.example/private/", Options{HTTPClient: httpClient})

	err := client.Dispatch(context.Background(), 987654)
	assertProviderError(t, err)
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Errorf("errors.Unwrap(error) = %T %v, want nil", unwrapped, unwrapped)
	}
	if errors.Is(err, privateCause) {
		t.Error("errors.Is(error, private cause) = true")
	}
	var recovered interface{ Error() string }
	if errors.As(err, &recovered) && recovered == privateCause {
		t.Error("errors.As recovered private cause")
	}
	assertRedacted(t, err, endpointCanary, documentCanary, "987654", bodyCanary, credential)
}

func TestDispatchStatusErrorRetainsOnlySafeStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(writer, "canary private response body")
	}))
	defer server.Close()
	client := newTestClient(t, server.URL+"/secret-endpoint", "https://paperless.example/private/")

	err := client.Dispatch(context.Background(), 987654)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("errors.As(*StatusError) = false: %v", err)
	}
	if classified, ok := any(statusErr).(interface{ RetrySafe() bool }); ok {
		t.Errorf("StatusError unexpectedly classifies retry safety as %t", classified.RetrySafe())
	}
	if unwrapped := errors.Unwrap(statusErr); unwrapped != nil {
		t.Errorf("errors.Unwrap(StatusError) = %v, want nil", unwrapped)
	}
	assertRedacted(t, err, server.URL, "secret-endpoint", "paperless.example", "987654", "canary private response body")
}

func TestNewRejectsMalformedConfiguration(t *testing.T) {
	tests := []struct {
		webhook   string
		paperless string
		key       string
	}{
		{webhook: "ftp://webhook.example", paperless: "https://paperless.example/", key: testWebhookKey},
		{webhook: "https://user@webhook.example", paperless: "https://paperless.example/", key: testWebhookKey},
		{webhook: "https://webhook.example/#fragment", paperless: "https://paperless.example/", key: testWebhookKey},
		{webhook: "https://webhook.example/", paperless: "ftp://paperless.example/", key: testWebhookKey},
		{webhook: "https://webhook.example/", paperless: "https://user@paperless.example/", key: testWebhookKey},
		{webhook: "https://webhook.example/", paperless: "https://paperless.example/#fragment", key: testWebhookKey},
		{webhook: "https://webhook.example/", paperless: "https://paperless.example/", key: " "},
	}
	for index, test := range tests {
		webhook, _ := url.Parse(test.webhook)
		paperless, _ := url.Parse(test.paperless)
		if _, err := New(webhook, paperless, test.key, Options{}); err == nil {
			t.Errorf("case %d New() error = nil", index)
		}
	}
}

func newTestClient(t *testing.T, webhook, paperless string) *Client {
	t.Helper()
	return newTestClientWithOptions(t, webhook, paperless, Options{})
}

func newTestClientWithOptions(t *testing.T, webhook, paperless string, options Options) *Client {
	t.Helper()
	webhookURL, err := url.Parse(webhook)
	if err != nil {
		t.Fatal(err)
	}
	paperlessURL, err := url.Parse(paperless)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(webhookURL, paperlessURL, testWebhookKey, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func assertProviderError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want provider error")
	}
	var safeErr *saferr.Error
	if !errors.As(err, &safeErr) || safeErr.Category() != saferr.CategoryProvider {
		t.Fatalf("error = %v, want provider category", err)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type countingBody struct {
	remaining int64
	read      atomic.Int64
}

func (body *countingBody) Read(buffer []byte) (int, error) {
	if body.remaining == 0 {
		return 0, io.EOF
	}
	read := min(int64(len(buffer)), body.remaining)
	for index := range int(read) {
		buffer[index] = 'x'
	}
	body.remaining -= read
	body.read.Add(read)
	return int(read), nil
}

func (*countingBody) Close() error { return nil }
