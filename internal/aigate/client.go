// Package aigate provides bounded access to an OpenAI-compatible Responses API.
package aigate

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	defaultRequestTimeout   = 30 * time.Second
	defaultMaxResponseBytes = int64(64 << 10)
)

// ClientOptions configures HTTP bounds. Zero values select safe defaults.
type ClientOptions struct {
	HTTPClient       *http.Client
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

// Client probes the configured Responses API's attachment capabilities.
type Client struct {
	responsesURL     *url.URL
	apiKey           string
	model            string
	httpClient       *http.Client
	requestTimeout   time.Duration
	maxResponseBytes int64

	mu         sync.Mutex
	cached     bool
	capability Capability
	terminal   bool
	flight     *probeFlight
}

type probeFlight struct {
	done       chan struct{}
	capability Capability
	err        error
}

// New creates a bounded Responses API client.
func New(baseURL *url.URL, apiKey, model string, options ClientOptions) (*Client, error) {
	if baseURL == nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Hostname() == "" || baseURL.User != nil || baseURL.Fragment != "" {
		return nil, providerError("base URL must be an http or https URL without userinfo or a fragment")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, providerError("API key must not be blank")
	}
	if strings.TrimSpace(model) == "" {
		return nil, providerError("model must not be blank")
	}
	if options.RequestTimeout < 0 || options.MaxResponseBytes < 0 {
		return nil, providerError("client limits must not be negative")
	}

	clonedURL := *baseURL
	clonedURL.RawQuery = ""
	clonedURL.RawPath = ""
	clonedURL.Path = normalizedBasePath(clonedURL.Path)
	responsesURL := clonedURL.ResolveReference(&url.URL{Path: "responses"})

	var httpClient *http.Client
	var callerPolicy func(*http.Request, []*http.Request) error
	if options.HTTPClient == nil {
		httpClient = &http.Client{Transport: boundedTransport()}
	} else {
		clone := *options.HTTPClient
		httpClient = &clone
		callerPolicy = options.HTTPClient.CheckRedirect
		if httpClient.Transport == nil {
			httpClient.Transport = boundedTransport()
		}
	}
	httpClient.CheckRedirect = redirectPolicy(&clonedURL, callerPolicy)

	return &Client{
		responsesURL:     responsesURL,
		apiKey:           apiKey,
		model:            model,
		httpClient:       httpClient,
		requestTimeout:   defaultValue(options.RequestTimeout, defaultRequestTimeout),
		maxResponseBytes: defaultValue(options.MaxResponseBytes, defaultMaxResponseBytes),
	}, nil
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

func redirectPolicy(baseURL *url.URL, callerPolicy func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		request.Header.Del("Authorization")
		defer request.Header.Del("Authorization")
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if len(via) == 0 || !allowedURL(baseURL, request.URL) {
			return errors.New("redirect rejected")
		}
		if callerPolicy != nil {
			return callerPolicy(request, via)
		}
		return nil
	}
}

func allowedURL(baseURL, candidate *url.URL) bool {
	if candidate == nil || candidate.User != nil || candidate.Fragment != "" || !strings.EqualFold(baseURL.Scheme, candidate.Scheme) || !strings.EqualFold(baseURL.Host, candidate.Host) {
		return false
	}
	if strings.Contains(candidate.EscapedPath(), "%") {
		return false
	}
	basePath := strings.TrimSuffix(baseURL.Path, "/")
	candidatePath := path.Clean(candidate.Path)
	return basePath == "" || basePath == "/" || candidatePath == basePath || strings.HasPrefix(candidatePath, basePath+"/")
}

func normalizedBasePath(basePath string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(basePath, "/"))
	if cleaned == "/" {
		return cleaned
	}
	return cleaned + "/"
}

func defaultValue[T time.Duration | int64](value, fallback T) T {
	if value == 0 {
		return fallback
	}
	return value
}

func providerError(message string) error {
	return saferr.New(saferr.CategoryProvider, message)
}

func providerContextError(message string, err error) error {
	if errors.Is(err, context.Canceled) {
		return saferr.Wrap(saferr.CategoryProvider, message, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return saferr.Wrap(saferr.CategoryProvider, message, context.DeadlineExceeded)
	}
	return providerError(message)
}
