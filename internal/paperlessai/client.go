// Package paperlessai dispatches completed documents to Paperless AI.
package paperlessai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	defaultRequestTimeout        = 30 * time.Second
	defaultMaxResponseDrainBytes = int64(4 << 10)
)

// Options configures HTTP bounds. Zero values select safe defaults.
type Options struct {
	HTTPClient            *http.Client
	RequestTimeout        time.Duration
	MaxResponseDrainBytes int64
}

// Client dispatches one document identifier to Paperless AI.
type Client struct {
	webhookURL            *url.URL
	paperlessURL          *url.URL
	webhookKey            string
	httpClient            *http.Client
	requestTimeout        time.Duration
	maxResponseDrainBytes int64
}

// StatusError reports an unexpected status without retaining response data.
type StatusError struct {
	StatusCode int
}

func (err *StatusError) Error() string {
	return fmt.Sprintf("paperless AI dispatch returned HTTP status %d", err.StatusCode)
}

func (err *StatusError) Format(state fmt.State, verb rune) {
	switch verb {
	case 's', 'v':
		io.WriteString(state, err.Error())
	case 'q':
		fmt.Fprintf(state, "%q", err.Error())
	default:
		fmt.Fprintf(state, "%%!%c(*paperlessai.StatusError=%s)", verb, err.Error())
	}
}

// New creates a bounded client using separate Paperless and webhook origins.
func New(webhookURL, paperlessURL *url.URL, webhookKey string, options Options) (*Client, error) {
	if !validURL(webhookURL) || !validURL(paperlessURL) || strings.TrimSpace(webhookKey) == "" ||
		options.RequestTimeout < 0 || options.MaxResponseDrainBytes < 0 {
		return nil, saferr.New(saferr.CategoryConfiguration, "invalid Paperless AI client configuration")
	}
	webhook := cloneURL(webhookURL)
	paperless := cloneURL(paperlessURL)
	paperless.RawQuery = ""
	paperless.Path = normalizedBasePath(paperless.Path)

	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: boundedTransport()}
	} else {
		clone := *httpClient
		httpClient = &clone
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("redirect rejected")
	}

	return &Client{
		webhookURL:            webhook,
		paperlessURL:          paperless,
		webhookKey:            webhookKey,
		httpClient:            httpClient,
		requestTimeout:        defaultValue(options.RequestTimeout, defaultRequestTimeout),
		maxResponseDrainBytes: defaultValue(options.MaxResponseDrainBytes, defaultMaxResponseDrainBytes),
	}, nil
}

// Dispatch invokes the configured webhook for one Paperless document.
func (client *Client) Dispatch(ctx context.Context, documentID int) error {
	if documentID <= 0 {
		return providerError(errors.New("invalid document ID"))
	}
	documentURL := client.paperlessURL.ResolveReference(&url.URL{Path: fmt.Sprintf("documents/%d/", documentID)})
	body, err := json.Marshal(struct {
		URL string `json:"url"`
	}{URL: documentURL.String()})
	if err != nil {
		return providerError(err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.webhookURL.String(), bytes.NewReader(body))
	if err != nil {
		return providerError(err)
	}
	request.Header.Set("x-api-key", client.webhookKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return providerError(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, client.maxResponseDrainBytes))
	if response.StatusCode != http.StatusAccepted {
		return providerError(&StatusError{StatusCode: response.StatusCode})
	}
	return nil
}

func validURL(value *url.URL) bool {
	return value != nil && (value.Scheme == "http" || value.Scheme == "https") && value.Hostname() != "" && value.User == nil && value.Fragment == ""
}

func cloneURL(value *url.URL) *url.URL {
	clone := *value
	clone.RawPath = ""
	return &clone
}

func normalizedBasePath(basePath string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(basePath, "/"))
	if cleaned == "/" {
		return cleaned
	}
	return cleaned + "/"
}

func boundedTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func providerError(cause error) error {
	return saferr.Wrap(saferr.CategoryProvider, "Paperless AI dispatch failed", cause)
}

func defaultValue[T time.Duration | int64](value, fallback T) T {
	if value == 0 {
		return fallback
	}
	return value
}
