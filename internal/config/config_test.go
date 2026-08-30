package config

import (
	"math"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

var requiredEnvironment = map[string]string{
	"PAPERLESS_URL":            "https://paperless.example.test",
	"PAPERLESS_API_TOKEN":      "paperless-secret",
	"AI_BASE_URL":              "https://ai.example.test/v1",
	"AI_API_KEY":               "ai-secret",
	"AI_MODEL":                 "vision-model",
	"WEBHOOK_TOKEN":            "webhook-secret",
	"PAPERLESS_AI_WEBHOOK_URL": "http://paperless-ai:3000/api/webhook/manual",
	"PAPERLESS_AI_WEBHOOK_KEY": "paperless-ai-secret",
}

var optionalEnvironment = []string{
	"HTTP_PORT",
	"POLL_INTERVAL",
	"RENDER_DPI",
	"BATCH_SIZE",
	"MODEL_ATTEMPTS",
	"RENDER_TIMEOUT",
	"MODEL_TIMEOUT",
	"DOCUMENT_DEADLINE",
	"TEMPORARY_RENDER_BUDGET",
}

func setValidEnvironment(t *testing.T) {
	t.Helper()

	for name, value := range requiredEnvironment {
		t.Setenv(name, value)
	}
	for _, name := range optionalEnvironment {
		t.Setenv(name, "")
	}
}

func TestLoadRequiresEnvironment(t *testing.T) {
	for name := range requiredEnvironment {
		t.Run(name, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv(name, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want missing %s error", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("Load() error = %q, want environment variable name", err)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	setValidEnvironment(t)

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got, want := config.HTTPPort, 8080; got != want {
		t.Errorf("HTTPPort = %d, want %d", got, want)
	}
	if got, want := config.PollInterval, 15*time.Minute; got != want {
		t.Errorf("PollInterval = %s, want %s", got, want)
	}
	if got, want := config.RenderDPI, 200; got != want {
		t.Errorf("RenderDPI = %d, want %d", got, want)
	}
	if got, want := config.BatchSize, 5; got != want {
		t.Errorf("BatchSize = %d, want %d", got, want)
	}
	if got, want := config.ModelAttempts, 3; got != want {
		t.Errorf("ModelAttempts = %d, want %d", got, want)
	}
	if got, want := config.RenderTimeout, 5*time.Minute; got != want {
		t.Errorf("RenderTimeout = %s, want %s", got, want)
	}
	if got, want := config.ModelTimeout, 3*time.Minute; got != want {
		t.Errorf("ModelTimeout = %s, want %s", got, want)
	}
	if got, want := config.DocumentDeadline, 6*time.Hour; got != want {
		t.Errorf("DocumentDeadline = %s, want %s", got, want)
	}
	if got, want := config.TemporaryRenderBudget, int64(1<<30); got != want {
		t.Errorf("TemporaryRenderBudget = %d, want %d", got, want)
	}
	if got, want := config.ActiveDocuments, 1; got != want {
		t.Errorf("ActiveDocuments = %d, want %d", got, want)
	}
	if got, want := config.ActiveModelRequests, 1; got != want {
		t.Errorf("ActiveModelRequests = %d, want %d", got, want)
	}
}

func TestLoadParsesRequiredValues(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("PAPERLESS_API_TOKEN", " paperless-secret ")
	t.Setenv("AI_API_KEY", " ai-secret ")
	t.Setenv("WEBHOOK_TOKEN", " webhook-secret ")
	t.Setenv("PAPERLESS_AI_WEBHOOK_KEY", " paperless-ai-secret ")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	assertURL(t, config.PaperlessURL, requiredEnvironment["PAPERLESS_URL"])
	assertURL(t, config.AIBaseURL, requiredEnvironment["AI_BASE_URL"])
	assertURL(t, config.PaperlessAIWebhookURL, requiredEnvironment["PAPERLESS_AI_WEBHOOK_URL"])
	if got, want := config.PaperlessAPIToken, " paperless-secret "; got != want {
		t.Errorf("PaperlessAPIToken = %q, want preserved value", got)
	}
	if got, want := config.AIAPIKey, " ai-secret "; got != want {
		t.Errorf("AIAPIKey = %q, want preserved value", got)
	}
	if got, want := config.AIModel, requiredEnvironment["AI_MODEL"]; got != want {
		t.Errorf("AIModel = %q, want %q", got, want)
	}
	if got, want := config.WebhookToken, " webhook-secret "; got != want {
		t.Errorf("WebhookToken = %q, want preserved value", got)
	}
	if got, want := config.PaperlessAIWebhookKey, " paperless-ai-secret "; got != want {
		t.Errorf("PaperlessAIWebhookKey = %q, want preserved value", got)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	setValidEnvironment(t)
	overrides := map[string]string{
		"HTTP_PORT":               "9090",
		"POLL_INTERVAL":           "30m",
		"RENDER_DPI":              "300",
		"BATCH_SIZE":              "4",
		"MODEL_ATTEMPTS":          "4",
		"RENDER_TIMEOUT":          "90s",
		"MODEL_TIMEOUT":           "2m30s",
		"DOCUMENT_DEADLINE":       "90m",
		"TEMPORARY_RENDER_BUDGET": "1536MiB",
	}
	for name, value := range overrides {
		t.Setenv(name, value)
	}

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got, want := config.HTTPPort, 9090; got != want {
		t.Errorf("HTTPPort = %d, want %d", got, want)
	}
	if got, want := config.PollInterval, 30*time.Minute; got != want {
		t.Errorf("PollInterval = %s, want %s", got, want)
	}
	if got, want := config.RenderDPI, 300; got != want {
		t.Errorf("RenderDPI = %d, want %d", got, want)
	}
	if got, want := config.BatchSize, 4; got != want {
		t.Errorf("BatchSize = %d, want %d", got, want)
	}
	if got, want := config.ModelAttempts, 4; got != want {
		t.Errorf("ModelAttempts = %d, want %d", got, want)
	}
	if got, want := config.RenderTimeout, 90*time.Second; got != want {
		t.Errorf("RenderTimeout = %s, want %s", got, want)
	}
	if got, want := config.ModelTimeout, 150*time.Second; got != want {
		t.Errorf("ModelTimeout = %s, want %s", got, want)
	}
	if got, want := config.DocumentDeadline, 90*time.Minute; got != want {
		t.Errorf("DocumentDeadline = %s, want %s", got, want)
	}
	if got, want := config.TemporaryRenderBudget, int64(1536<<20); got != want {
		t.Errorf("TemporaryRenderBudget = %d, want %d", got, want)
	}
}

func TestLoadAcceptsBoundedModelAttempts(t *testing.T) {
	for _, value := range []string{"1", "10"} {
		t.Run(value, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv("MODEL_ATTEMPTS", value)

			config, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			want, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("strconv.Atoi(%q) error = %v", value, err)
			}
			if config.ModelAttempts != want {
				t.Errorf("ModelAttempts = %d, want %d", config.ModelAttempts, want)
			}
		})
	}
}

func TestLoadRejectsInvalidScalarOverrides(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "HTTP_PORT", value: "0"},
		{name: "HTTP_PORT", value: "65536"},
		{name: "RENDER_DPI", value: "-1"},
		{name: "BATCH_SIZE", value: "zero"},
		{name: "BATCH_SIZE", value: "6"},
		{name: "MODEL_ATTEMPTS", value: "0"},
		{name: "MODEL_ATTEMPTS", value: "11"},
		{name: "MODEL_ATTEMPTS", value: strconv.Itoa(math.MaxInt)},
	}

	for _, test := range tests {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv(test.name, test.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want invalid %s error", test.name)
			}
			if !strings.Contains(err.Error(), test.name) {
				t.Errorf("Load() error = %q, want environment variable name", err)
			}
		})
	}
}

func TestLoadRejectsInvalidDurations(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "POLL_INTERVAL", value: "later"},
		{name: "POLL_INTERVAL", value: "0s"},
		{name: "RENDER_TIMEOUT", value: "-1s"},
		{name: "MODEL_TIMEOUT", value: "999999999999999999999h"},
		{name: "DOCUMENT_DEADLINE", value: "0"},
	}

	for _, test := range tests {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv(test.name, test.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want invalid %s error", test.name)
			}
			if !strings.Contains(err.Error(), test.name) {
				t.Errorf("Load() error = %q, want environment variable name", err)
			}
		})
	}
}

func TestLoadParsesByteSizes(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{value: "512B", want: 512},
		{value: "2KiB", want: 2 << 10},
		{value: "3MiB", want: 3 << 20},
		{value: "4GiB", want: 4 << 30},
	}

	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv("TEMPORARY_RENDER_BUDGET", test.value)

			config, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if config.TemporaryRenderBudget != test.want {
				t.Errorf("TemporaryRenderBudget = %d, want %d", config.TemporaryRenderBudget, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidByteSizes(t *testing.T) {
	tests := []string{"", "0B", "-1MiB", "1MB", "1.5GiB", "9223372036854775807GiB"}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv("TEMPORARY_RENDER_BUDGET", value)
			if value == "" {
				t.Setenv("TEMPORARY_RENDER_BUDGET", "0B")
			}

			_, err := Load()
			if err == nil {
				t.Fatal("Load() error = nil, want invalid TEMPORARY_RENDER_BUDGET error")
			}
			if !strings.Contains(err.Error(), "TEMPORARY_RENDER_BUDGET") {
				t.Errorf("Load() error = %q, want environment variable name", err)
			}
		})
	}
}

func TestLoadRejectsInvalidURLs(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "PAPERLESS_URL", value: "ftp://paperless.example.test"},
		{name: "PAPERLESS_URL", value: "https://user:password@paperless.example.test"},
		{name: "AI_BASE_URL", value: "://malformed"},
		{name: "AI_BASE_URL", value: "https://ai.example.test/v1#fragment"},
		{name: "PAPERLESS_AI_WEBHOOK_URL", value: "https://"},
		{name: "PAPERLESS_AI_WEBHOOK_URL", value: " paperless-ai "},
		{name: "PAPERLESS_URL", value: "https://:8080"},
		{name: "AI_BASE_URL", value: "https://:8080"},
		{name: "PAPERLESS_AI_WEBHOOK_URL", value: "https://:8080"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setValidEnvironment(t)
			t.Setenv(test.name, test.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want invalid %s error", test.name)
			}
			if !strings.Contains(err.Error(), test.name) {
				t.Errorf("Load() error = %q, want environment variable name", err)
			}
		})
	}
}

func TestLoadRejectsBlankTokensAndModelWithoutDisclosingValues(t *testing.T) {
	tests := []string{
		"PAPERLESS_API_TOKEN",
		"AI_API_KEY",
		"AI_MODEL",
		"WEBHOOK_TOKEN",
		"PAPERLESS_AI_WEBHOOK_KEY",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			setValidEnvironment(t)
			const secret = "   \t  "
			t.Setenv(name, secret)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want blank %s error", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("Load() error = %q, want environment variable name", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("Load() error disclosed configured value: %q", err)
			}
		})
	}
}

func TestLoadDoesNotExposeConfigurableConcurrency(t *testing.T) {
	setValidEnvironment(t)
	t.Setenv("ACTIVE_DOCUMENTS", "9")
	t.Setenv("ACTIVE_MODEL_REQUESTS", "9")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.ActiveDocuments != 1 || config.ActiveModelRequests != 1 {
		t.Fatalf("concurrency = documents:%d requests:%d, want fixed at one", config.ActiveDocuments, config.ActiveModelRequests)
	}
}

func assertURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatal("URL = nil")
	}
	if got.String() != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}
