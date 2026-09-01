// Package paperless provides the bounded Paperless API operations used by the service.
package paperless

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
	"slices"
	"strings"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	defaultRequestTimeout       = 30 * time.Second
	defaultMaxJSONResponseBytes = int64(8 << 20)
	defaultMaxDownloadBytes     = int64(1 << 30)
	defaultMaxPages             = 10_000
	maxErrorDrainBytes          = int64(4 << 10)
	downloadBufferBytes         = 32 << 10
	pingDrainBytes              = 4 << 10
)

// Options configures HTTP bounds. Zero values select safe defaults.
type Options struct {
	HTTPClient           *http.Client
	RequestTimeout       time.Duration
	MaxJSONResponseBytes int64
	MaxDownloadBytes     int64
	MaxPages             int
}

// Client performs the Paperless operations required by the service.
type Client struct {
	baseURL              *url.URL
	token                string
	httpClient           *http.Client
	requestTimeout       time.Duration
	maxJSONResponseBytes int64
	maxDownloadBytes     int64
	maxPages             int
	ensureTagGate        chan struct{}
}

// StatusError reports an HTTP response status without retaining response data.
type StatusError struct {
	Operation  string
	StatusCode int
}

func (err *StatusError) Error() string {
	return fmt.Sprintf("paperless %s returned HTTP status %d", err.Operation, err.StatusCode)
}

func (err *StatusError) Format(state fmt.State, verb rune) {
	switch verb {
	case 's', 'v':
		io.WriteString(state, err.Error())
	case 'q':
		fmt.Fprintf(state, "%q", err.Error())
	default:
		fmt.Fprintf(state, "%%!%c(*paperless.StatusError=%s)", verb, err.Error())
	}
}

// New creates a client without coupling it to application configuration.
func New(baseURL *url.URL, token string, options Options) (*Client, error) {
	if baseURL == nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Hostname() == "" || baseURL.User != nil || baseURL.Fragment != "" {
		return nil, saferr.New(saferr.CategoryPaperless, "base URL must be an http or https URL without userinfo or a fragment")
	}
	if strings.TrimSpace(token) == "" {
		return nil, saferr.New(saferr.CategoryPaperless, "API token must not be blank")
	}
	if options.RequestTimeout < 0 || options.MaxJSONResponseBytes < 0 || options.MaxDownloadBytes < 0 || options.MaxPages < 0 {
		return nil, saferr.New(saferr.CategoryPaperless, "client limits must not be negative")
	}

	clonedURL := *baseURL
	clonedURL.RawQuery = ""
	clonedURL.Path = normalizedBasePath(clonedURL.Path)
	clonedURL.RawPath = ""

	httpClient := options.HTTPClient
	var callerRedirectPolicy func(*http.Request, []*http.Request) error
	if httpClient == nil {
		httpClient = &http.Client{Transport: boundedTransport()}
	} else {
		callerRedirectPolicy = httpClient.CheckRedirect
		clone := *httpClient
		httpClient = &clone
	}
	httpClient.CheckRedirect = redirectPolicy(&clonedURL, callerRedirectPolicy)

	return &Client{
		baseURL:              &clonedURL,
		token:                token,
		httpClient:           httpClient,
		requestTimeout:       defaultValue(options.RequestTimeout, defaultRequestTimeout),
		maxJSONResponseBytes: defaultValue(options.MaxJSONResponseBytes, defaultMaxJSONResponseBytes),
		maxDownloadBytes:     defaultValue(options.MaxDownloadBytes, defaultMaxDownloadBytes),
		maxPages:             defaultValue(options.MaxPages, defaultMaxPages),
		ensureTagGate:        make(chan struct{}, 1),
	}, nil
}

// Ping verifies authenticated Paperless API connectivity without enumerating data.
func (client *Client) Ping(ctx context.Context) error {
	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	endpoint := client.endpoint("api/documents/")
	query := endpoint.Query()
	query.Set("page_size", "1")
	endpoint.RawQuery = query.Encode()
	request, err := client.request(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return paperlessError("ping", err)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return paperlessHTTPError("ping", err)
	}
	defer response.Body.Close()
	drainLimit(response.Body, pingDrainBytes)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return paperlessError("ping", &StatusError{Operation: "ping", StatusCode: response.StatusCode})
	}
	return nil
}

// WalkDocuments visits the archive one bounded API page at a time.
func (client *Client) WalkDocuments(ctx context.Context, visit func([]Document) error) error {
	if visit == nil {
		return paperlessError("walk documents", errors.New("nil page callback"))
	}
	next := client.endpoint("api/documents/")
	var callbackErr error
	err := client.walkPages(ctx, "walk documents", next, func(next *url.URL) (*string, error) {
		var page documentPage
		if err := client.doJSON(ctx, "walk documents", http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		for _, document := range page.Results {
			if err := validateDocument(document); err != nil {
				return nil, paperlessError("walk documents", err)
			}
		}
		if err := visit(page.Results); err != nil {
			callbackErr = err
			return nil, err
		}
		return page.Next, nil
	})
	if callbackErr != nil {
		return paperlessCallbackError(callbackErr)
	}
	if err != nil {
		return err
	}
	return nil
}

// ListDocumentsPage retrieves one archive page from a validated opaque cursor.
func (client *Client) ListDocumentsPage(ctx context.Context, cursor string) (DocumentPage, error) {
	current := client.endpoint("api/documents/")
	if cursor != "" {
		parsed, err := url.Parse(cursor)
		if err != nil || !parsed.IsAbs() || !allowedURL(client.baseURL, parsed) {
			return DocumentPage{}, paperlessError("list documents page", errors.New("invalid pagination cursor"))
		}
		current = parsed
	}

	var page documentPage
	if err := client.doJSON(ctx, "list documents page", http.MethodGet, current, nil, &page); err != nil {
		return DocumentPage{}, err
	}
	for _, document := range page.Results {
		if err := validateDocument(document); err != nil {
			return DocumentPage{}, paperlessError("list documents page", err)
		}
	}
	next, err := client.nextURL(current, page.Next)
	if err != nil {
		return DocumentPage{}, paperlessError("list documents page", err)
	}
	result := DocumentPage{Documents: page.Results}
	if next != nil {
		result.Next = next.String()
	}
	return result, nil
}

// GetDocument retrieves the Paperless detail fields used by the worker.
func (client *Client) GetDocument(ctx context.Context, documentID int) (Document, error) {
	if documentID <= 0 {
		return Document{}, paperlessError("get document", errors.New("invalid document ID"))
	}
	var document Document
	if err := client.doJSON(ctx, "get document", http.MethodGet, client.documentEndpoint(documentID, ""), nil, &document); err != nil {
		return Document{}, err
	}
	if err := validateDocument(document); err != nil {
		return Document{}, paperlessError("get document", err)
	}
	return document, nil
}

// GetChecksum retrieves the source checksum exposed on document detail.
func (client *Client) GetChecksum(ctx context.Context, documentID int) (string, error) {
	document, err := client.GetDocument(ctx, documentID)
	if err != nil {
		return "", err
	}
	return document.Checksum, nil
}

// DownloadOriginal streams the original source document to destination.
func (client *Client) DownloadOriginal(ctx context.Context, documentID int, destination io.Writer) error {
	if documentID <= 0 || destination == nil {
		return paperlessError("download original", errors.New("invalid download input"))
	}
	endpoint := client.documentEndpoint(documentID, "download/")
	query := endpoint.Query()
	query.Set("original", "true")
	endpoint.RawQuery = query.Encode()

	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	request, err := client.request(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return paperlessError("download original", err)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return paperlessHTTPError("download original", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		drain(response.Body)
		return paperlessError("download original", &StatusError{Operation: "download original", StatusCode: response.StatusCode})
	}

	written, err := io.CopyBuffer(destination, io.LimitReader(response.Body, client.maxDownloadBytes), make([]byte, downloadBufferBytes))
	if err != nil {
		return paperlessHTTPError("download original", err)
	}
	var probe [1]byte
	probeBytes, probeErr := response.Body.Read(probe[:])
	if probeErr != nil && !errors.Is(probeErr, io.EOF) {
		return paperlessHTTPError("download original", probeErr)
	}
	if written == client.maxDownloadBytes && probeBytes != 0 {
		return paperlessError("download original", errors.New("download response exceeded limit"))
	}
	return nil
}

// UpdateContent replaces only native Paperless document content.
func (client *Client) UpdateContent(ctx context.Context, documentID int, content string) error {
	if documentID <= 0 {
		return paperlessError("update content", errors.New("invalid document ID"))
	}
	return client.doJSON(ctx, "update content", http.MethodPatch, client.documentEndpoint(documentID, ""), struct {
		Content string `json:"content"`
	}{Content: content}, nil)
}

// ReplaceTags replaces only the Paperless tag IDs in deterministic order.
func (client *Client) ReplaceTags(ctx context.Context, documentID int, tagIDs []int) error {
	if documentID <= 0 {
		return paperlessError("replace tags", errors.New("invalid document ID"))
	}
	canonical, err := canonicalTagIDs(tagIDs)
	if err != nil {
		return paperlessError("replace tags", err)
	}
	return client.doJSON(ctx, "replace tags", http.MethodPatch, client.documentEndpoint(documentID, ""), struct {
		Tags []int `json:"tags"`
	}{Tags: canonical}, nil)
}

// UpdateTags preserves unrelated current tags, removes requested IDs, then adds IDs.
func (client *Client) UpdateTags(ctx context.Context, documentID int, current, add, remove []int) error {
	for _, ids := range [][]int{current, add, remove} {
		if _, err := canonicalTagIDs(ids); err != nil {
			return paperlessError("update tags", err)
		}
	}
	removed := make(map[int]struct{}, len(remove))
	for _, id := range remove {
		removed[id] = struct{}{}
	}
	result := make([]int, 0, len(current)+len(add))
	for _, id := range current {
		if _, found := removed[id]; !found {
			result = append(result, id)
		}
	}
	result = append(result, add...)
	return client.ReplaceTags(ctx, documentID, result)
}

// EnsureTag returns one exact-name tag or creates it when absent.
func (client *Client) EnsureTag(ctx context.Context, name string) (Tag, error) {
	if strings.TrimSpace(name) == "" {
		return Tag{}, paperlessError("ensure tag", errors.New("blank tag name"))
	}
	select {
	case client.ensureTagGate <- struct{}{}:
		defer func() { <-client.ensureTagGate }()
	case <-ctx.Done():
		return Tag{}, paperlessError("ensure tag", ctx.Err())
	}
	matches, err := client.lookupTags(ctx, name)
	if err != nil {
		return Tag{}, err
	}
	if len(matches) > 1 {
		return Tag{}, paperlessError("ensure tag", errors.New("ambiguous exact tag name"))
	}
	if len(matches) == 1 {
		return matches[0], nil
	}

	var created Tag
	err = client.doJSON(ctx, "ensure tag", http.MethodPost, client.endpoint("api/tags/"), struct {
		Name string `json:"name"`
	}{Name: name}, &created)
	if err != nil {
		createErr := err
		var statusErr *StatusError
		if !errors.As(createErr, &statusErr) {
			return Tag{}, createErr
		}
		matches, err = client.lookupTags(ctx, name)
		if err != nil || len(matches) != 1 {
			return Tag{}, createErr
		}
		return matches[0], nil
	}
	if err := validateTag(created); err != nil {
		return Tag{}, paperlessError("ensure tag", err)
	}
	return created, nil
}

func (client *Client) lookupTags(ctx context.Context, name string) ([]Tag, error) {
	endpoint := client.endpoint("api/tags/")
	query := endpoint.Query()
	query.Set("name__iexact", name)
	endpoint.RawQuery = query.Encode()

	matches := make([]Tag, 0, 1)
	if err := client.walkPages(ctx, "ensure tag", endpoint, func(next *url.URL) (*string, error) {
		var page tagPage
		if err := client.doJSON(ctx, "ensure tag", http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		for _, tag := range page.Results {
			if err := validateTag(tag); err != nil {
				return nil, paperlessError("ensure tag", err)
			}
			if tag.Name == name {
				matches = append(matches, tag)
			}
		}
		return page.Next, nil
	}); err != nil {
		return nil, err
	}
	return matches, nil
}

func (client *Client) walkPages(ctx context.Context, operation string, first *url.URL, visit func(*url.URL) (*string, error)) error {
	next := first
	visited := make(map[string]struct{})
	for pageNumber := 0; next != nil; pageNumber++ {
		if pageNumber >= client.maxPages {
			return paperlessError(operation, errors.New("pagination page limit exceeded"))
		}
		key := next.String()
		if _, found := visited[key]; found {
			return paperlessError(operation, errors.New("pagination loop detected"))
		}
		visited[key] = struct{}{}

		nextReference, err := visit(next)
		if err != nil {
			return err
		}
		next, err = client.nextURL(next, nextReference)
		if err != nil {
			return paperlessError(operation, err)
		}
	}
	return nil
}

func (client *Client) doJSON(ctx context.Context, operation, method string, endpoint *url.URL, requestBody, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return paperlessError(operation, err)
		}
		body = bytes.NewReader(encoded)
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.requestTimeout)
	defer cancel()
	request, err := client.request(requestCtx, method, endpoint, body)
	if err != nil {
		return paperlessError(operation, err)
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return paperlessHTTPError(operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		drain(response.Body)
		return paperlessError(operation, &StatusError{Operation: operation, StatusCode: response.StatusCode})
	}
	if responseBody == nil {
		drain(response.Body)
		return nil
	}
	if err := decodeLimitedJSON(response.Body, client.maxJSONResponseBytes, responseBody); err != nil {
		return paperlessHTTPError(operation, err)
	}
	return nil
}

func (client *Client) request(ctx context.Context, method string, endpoint *url.URL, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Token "+client.token)
	request.Header.Set("Accept", "application/json")
	return request, nil
}

func (client *Client) endpoint(relative string) *url.URL {
	return client.baseURL.ResolveReference(&url.URL{Path: relative})
}

func (client *Client) documentEndpoint(documentID int, suffix string) *url.URL {
	return client.endpoint(fmt.Sprintf("api/documents/%d/%s", documentID, suffix))
}

func (client *Client) nextURL(current *url.URL, next *string) (*url.URL, error) {
	if next == nil || *next == "" {
		return nil, nil
	}
	parsed, err := url.Parse(*next)
	if err != nil {
		return nil, errors.New("invalid pagination URL")
	}
	resolved := current.ResolveReference(parsed)
	if !allowedURL(client.baseURL, resolved) {
		return nil, errors.New("pagination URL escaped configured origin")
	}
	return resolved, nil
}

func canonicalTagIDs(tagIDs []int) ([]int, error) {
	canonical := slices.Clone(tagIDs)
	if canonical == nil {
		canonical = []int{}
	}
	for _, id := range canonical {
		if id <= 0 {
			return nil, errors.New("tag IDs must be positive")
		}
	}
	slices.Sort(canonical)
	return slices.Compact(canonical), nil
}

func validateDocument(document Document) error {
	if document.ID <= 0 {
		return errors.New("Paperless returned an invalid document")
	}
	for _, tagID := range document.Tags {
		if tagID <= 0 {
			return errors.New("Paperless returned an invalid document")
		}
	}
	return nil
}

func validateTag(tag Tag) error {
	if tag.ID <= 0 {
		return errors.New("Paperless returned an invalid tag")
	}
	return nil
}

func decodeLimitedJSON(reader io.Reader, limit int64, destination any) error {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("JSON response exceeded limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON response contained trailing data")
	}
	return nil
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
		if len(via) == 0 || (via[0].Method != http.MethodGet && via[0].Method != http.MethodHead) {
			return errors.New("redirect rejected for mutating request")
		}
		if !allowedURL(baseURL, request.URL) {
			return errors.New("cross-origin redirect rejected")
		}
		if callerPolicy != nil {
			return callerPolicy(request, via)
		}
		return nil
	}
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func allowedURL(baseURL, candidate *url.URL) bool {
	if candidate == nil || !sameOrigin(baseURL, candidate) || candidate.User != nil || candidate.Fragment != "" {
		return false
	}
	escapedPath := candidate.EscapedPath()
	if strings.Contains(escapedPath, "%") {
		return false
	}
	candidatePath := path.Clean(candidate.Path)
	basePath := strings.TrimSuffix(baseURL.Path, "/")
	if basePath == "" {
		return strings.HasPrefix(candidatePath, "/")
	}
	return candidatePath == basePath || strings.HasPrefix(candidatePath, basePath+"/")
}

func normalizedBasePath(basePath string) string {
	cleaned := path.Clean("/" + strings.TrimPrefix(basePath, "/"))
	if cleaned == "/" {
		return cleaned
	}
	return cleaned + "/"
}

func drain(reader io.Reader) {
	drainLimit(reader, maxErrorDrainBytes)
}

func drainLimit(reader io.Reader, limit int64) {
	_, _ = io.Copy(io.Discard, io.LimitReader(reader, limit))
}

func paperlessError(operation string, cause error) error {
	return saferr.Wrap(saferr.CategoryPaperless, operation+" failed", cause)
}

func paperlessHTTPError(operation string, cause error) error {
	if errors.Is(cause, context.Canceled) {
		return saferr.Wrap(saferr.CategoryPaperless, operation+" failed", context.Canceled)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return saferr.Wrap(saferr.CategoryPaperless, operation+" failed", context.DeadlineExceeded)
	}
	var statusError *StatusError
	if errors.As(cause, &statusError) {
		return saferr.Wrap(saferr.CategoryPaperless, operation+" failed", statusError)
	}
	return saferr.New(saferr.CategoryPaperless, operation+" failed")
}

func paperlessCallbackError(cause error) error {
	if errors.Is(cause, context.Canceled) {
		return saferr.Wrap(saferr.CategoryPaperless, "walk documents callback failed", context.Canceled)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return saferr.Wrap(saferr.CategoryPaperless, "walk documents callback failed", context.DeadlineExceeded)
	}
	return saferr.New(saferr.CategoryPaperless, "walk documents callback failed")
}

func defaultValue[T time.Duration | int64 | int](value, fallback T) T {
	if value == 0 {
		return fallback
	}
	return value
}
