# AI Gateway Capability Probe Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Probe and cache whether the configured Responses API model supports direct PDF input or page-image input, without exposing credentials or provider bodies.

**Architecture:** Add an `internal/aigate` Responses API client with bounded HTTP handling and a concurrency-safe probe state machine. Probe a deterministic embedded PDF first, fall back to its PNG only for structured unsupported-file responses, and cache successful or terminal unsupported results while allowing transient failures to retry.

**Tech Stack:** Go 1.26 standard library, `net/http`, `encoding/json`, `embed`, fake HTTP servers, OpenAI-compatible `POST /v1/responses`.

---

### Task 1: Add Public Synthetic Probe Fixtures

**Files:**
- Create: `testdata/probe/capability.pdf`
- Create: `testdata/probe/capability.png`
- Create: `internal/aigate/capability_test.go`

**Steps:**
1. Create one tiny one-page PDF containing a large fixed visual nonce and a PNG rendered from the same page. The nonce must not occur in either filename or probe prompt.
2. Write a failing test that loads both embedded fixtures, verifies their signatures, and verifies the nonce is absent from the fixed prompt.
3. Run the focused test and confirm RED because embedding and constants do not exist.
4. Add the minimal `go:embed` declarations and constants required for GREEN.

### Task 2: Define Client and Capability API

**Files:**
- Create: `internal/aigate/client.go`
- Create: `internal/aigate/capability.go`
- Modify: `internal/aigate/capability_test.go`

**Steps:**
1. Write failing tests for `Capability`, `DirectPDF`, `PageImages`, constructor validation, defaults, URL normalization, API-key/model validation, nil context, and redirect boundaries.
2. Run focused tests and confirm RED.
3. Implement `ClientOptions`, immutable `Client`, `New`, normalized `responses` endpoint, cloned HTTP client, bounded transport defaults, and same-origin/base-path redirect policy.
4. Run focused tests and confirm GREEN.

Intended API:

```go
type Capability string

const (
	DirectPDF  Capability = "direct_pdf"
	PageImages Capability = "page_images"
)

type ClientOptions struct {
	HTTPClient       *http.Client
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

func New(baseURL *url.URL, apiKey, model string, options ClientOptions) (*Client, error)
func (client *Client) Probe(ctx context.Context) (Capability, error)
```

### Task 3: Probe Direct PDF Through Responses API

**Files:**
- Modify: `internal/aigate/client.go`
- Modify: `internal/aigate/capability.go`
- Modify: `internal/aigate/capability_test.go`

**Steps:**
1. Write fake-server tests that inspect the exact PDF request: bearer auth, configured model, `input_file`, `probe.pdf`, base64 PDF data URL, `detail: low`, fixed instruction without nonce, and bounded output tokens.
2. Add tests for a valid Responses envelope containing exactly the visual nonce and for malformed, trailing, excessive, refusal, missing, duplicate, and wrong output text.
3. Confirm RED.
4. Implement bounded request encoding, timeout/cancellation, bounded response reading, strict single-JSON-value decoding, and strict extraction of one matching `output_text`.
5. Confirm GREEN.

### Task 4: Classify Provider Errors and Probe Images

**Files:**
- Modify: `internal/aigate/client.go`
- Modify: `internal/aigate/capability.go`
- Modify: `internal/aigate/capability_test.go`

**Steps:**
1. Write failing tests for structured PDF unsupported errors followed by a valid `input_image` request and `PageImages` selection.
2. Test terminal unsupported for both transports.
3. Test that auth failures, 429, 5xx, timeout, network failure, malformed error envelopes, unknown codes, and unrelated params do not trigger fallback.
4. Test API key, base URL, raw provider body, `error.message`, request content, fixture bytes, and canaries are absent from every public error-chain format.
5. Confirm RED.
6. Decode only bounded `error.type`, `error.code`, and `error.param`; never retain raw body or message. Use an explicit allowlist of unsupported multimodal codes/params and only HTTP 400/422.
7. Implement the image request and confirm GREEN.

### Task 5: Add Process-Lifetime Probe Caching

**Files:**
- Modify: `internal/aigate/capability.go`
- Modify: `internal/aigate/capability_test.go`

**Steps:**
1. Write deterministic failing tests for concurrent callers sharing one request sequence, cached `DirectPDF`, cached `PageImages`, cached terminal unsupported, transient retry, leader cancellation retry, and waiter cancellation not canceling the leader.
2. Confirm RED under `-race`.
3. Implement a mutex-protected state machine with one active leader and a completion channel. Cache only successful capability and confirmed terminal unsupported; do not cache cancellation, timeout, 429, 5xx, network errors, or malformed responses.
4. Confirm GREEN under `-race`.

### Task 6: Refactor and Verify

**Files:**
- Review all `internal/aigate` files and probe fixtures.

**Steps:**
1. Refactor only while green; keep provider protocol types private and narrow.
2. Run:

```bash
go test -count=50 -race ./internal/aigate
go test -count=1 -race ./...
go vet ./...
git diff --check
GOOS=linux GOARCH=amd64 go test -c -o /tmp/paperless-ai-ocr-aigate-amd64.test ./internal/aigate
GOOS=linux GOARCH=arm64 go test -c -o /tmp/paperless-ai-ocr-aigate-arm64.test ./internal/aigate
```

3. Require independent `SPEC_COMPLIANT` review against Task 10 and this plan.
4. Require independent quality review returning `APPROVED`; fix and re-review all blocking findings.
5. Re-run every verification command freshly.
6. Commit without amending:

```bash
git add docs/plans/2026-08-29-task-10-ai-gateway-capability-probe.md internal/aigate/client.go internal/aigate/capability.go internal/aigate/capability_test.go testdata/probe/
git commit -m "feat: probe multimodal gateway capabilities"
git push origin feature/initial-implementation
```
