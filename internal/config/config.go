// Package config loads and validates service configuration from the environment.
package config

import (
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nosovk/paperless-ai-ocr/internal/saferr"
)

const (
	defaultHTTPPort                  = 8080
	defaultPollInterval              = 15 * time.Minute
	defaultRenderDPI                 = 200
	defaultBatchSize                 = 5
	defaultModelAttempts             = 3
	maximumModelAttempts             = 10
	defaultRenderTimeout             = 5 * time.Minute
	defaultModelTimeout              = 3 * time.Minute
	defaultPaperlessAIWebhookTimeout = 30 * time.Second
	defaultDocumentDeadline          = 6 * time.Hour
	defaultTemporaryRenderBudget     = int64(1 << 30)
	fixedActiveDocuments             = 1
	fixedActiveModelRequests         = 1
)

// Config is the validated runtime configuration. Concurrency is intentionally
// fixed at one and cannot be changed through the environment.
type Config struct {
	PaperlessURL              *url.URL
	PaperlessAPIToken         string
	AIBaseURL                 *url.URL
	AIAPIKey                  string
	AIModel                   string
	WebhookToken              string
	PaperlessAIWebhookURL     *url.URL
	PaperlessAIWebhookKey     string
	HTTPPort                  int
	PollInterval              time.Duration
	RenderDPI                 int
	BatchSize                 int
	ModelAttempts             int
	RenderTimeout             time.Duration
	ModelTimeout              time.Duration
	PaperlessAIWebhookTimeout time.Duration
	DocumentDeadline          time.Duration
	TemporaryRenderBudget     int64
	ActiveDocuments           int
	ActiveModelRequests       int
}

// Load reads configuration from environment variables. Byte sizes use a
// positive integer followed by B, KiB, MiB, or GiB.
func Load() (Config, error) {
	paperlessURL, err := requiredURL("PAPERLESS_URL")
	if err != nil {
		return Config{}, err
	}
	paperlessAPIToken, err := requiredNonblank("PAPERLESS_API_TOKEN")
	if err != nil {
		return Config{}, err
	}
	aiBaseURL, err := requiredURL("AI_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	aiAPIKey, err := requiredNonblank("AI_API_KEY")
	if err != nil {
		return Config{}, err
	}
	aiModel, err := requiredNonblank("AI_MODEL")
	if err != nil {
		return Config{}, err
	}
	webhookToken, err := requiredNonblank("WEBHOOK_TOKEN")
	if err != nil {
		return Config{}, err
	}
	paperlessAIWebhookURL, err := requiredURL("PAPERLESS_AI_WEBHOOK_URL")
	if err != nil {
		return Config{}, err
	}
	paperlessAIWebhookKey, err := requiredNonblank("PAPERLESS_AI_WEBHOOK_KEY")
	if err != nil {
		return Config{}, err
	}

	httpPort, err := positiveInt("HTTP_PORT", defaultHTTPPort, 65535)
	if err != nil {
		return Config{}, err
	}
	pollInterval, err := positiveDuration("POLL_INTERVAL", defaultPollInterval)
	if err != nil {
		return Config{}, err
	}
	renderDPI, err := positiveInt("RENDER_DPI", defaultRenderDPI, math.MaxInt)
	if err != nil {
		return Config{}, err
	}
	batchSize, err := positiveInt("BATCH_SIZE", defaultBatchSize, 5)
	if err != nil {
		return Config{}, err
	}
	modelAttempts, err := positiveInt("MODEL_ATTEMPTS", defaultModelAttempts, maximumModelAttempts)
	if err != nil {
		return Config{}, err
	}
	renderTimeout, err := positiveDuration("RENDER_TIMEOUT", defaultRenderTimeout)
	if err != nil {
		return Config{}, err
	}
	modelTimeout, err := positiveDuration("MODEL_TIMEOUT", defaultModelTimeout)
	if err != nil {
		return Config{}, err
	}
	paperlessAIWebhookTimeout, err := positiveDuration("PAPERLESS_AI_WEBHOOK_TIMEOUT", defaultPaperlessAIWebhookTimeout)
	if err != nil {
		return Config{}, err
	}
	documentDeadline, err := positiveDuration("DOCUMENT_DEADLINE", defaultDocumentDeadline)
	if err != nil {
		return Config{}, err
	}
	temporaryRenderBudget, err := byteSize("TEMPORARY_RENDER_BUDGET", defaultTemporaryRenderBudget)
	if err != nil {
		return Config{}, err
	}

	return Config{
		PaperlessURL:              paperlessURL,
		PaperlessAPIToken:         paperlessAPIToken,
		AIBaseURL:                 aiBaseURL,
		AIAPIKey:                  aiAPIKey,
		AIModel:                   aiModel,
		WebhookToken:              webhookToken,
		PaperlessAIWebhookURL:     paperlessAIWebhookURL,
		PaperlessAIWebhookKey:     paperlessAIWebhookKey,
		HTTPPort:                  httpPort,
		PollInterval:              pollInterval,
		RenderDPI:                 renderDPI,
		BatchSize:                 batchSize,
		ModelAttempts:             modelAttempts,
		RenderTimeout:             renderTimeout,
		ModelTimeout:              modelTimeout,
		PaperlessAIWebhookTimeout: paperlessAIWebhookTimeout,
		DocumentDeadline:          documentDeadline,
		TemporaryRenderBudget:     temporaryRenderBudget,
		ActiveDocuments:           fixedActiveDocuments,
		ActiveModelRequests:       fixedActiveModelRequests,
	}, nil
}

func requiredURL(name string) (*url.URL, error) {
	value, err := requiredNonblank(name)
	if err != nil {
		return nil, err
	}
	parsed, parseErr := url.Parse(value)
	if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, configError(name + " must be an http or https URL without userinfo or a fragment")
	}
	return parsed, nil
}

func requiredNonblank(name string) (string, error) {
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", configError(name + " is required and must not be blank")
	}
	return value, nil
}

func positiveInt(name string, fallback, maximum int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > maximum {
		return 0, configError(name + " must be a positive integer in range")
	}
	return parsed, nil
}

func positiveDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, configError(name + " must be a positive Go duration")
	}
	return parsed, nil
}

func byteSize(name string, fallback int64) (int64, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}

	var multiplier int64
	var number string
	for _, unit := range []struct {
		suffix     string
		multiplier int64
	}{
		{suffix: "GiB", multiplier: 1 << 30},
		{suffix: "MiB", multiplier: 1 << 20},
		{suffix: "KiB", multiplier: 1 << 10},
		{suffix: "B", multiplier: 1},
	} {
		if prefix, ok := strings.CutSuffix(value, unit.suffix); ok {
			number = prefix
			multiplier = unit.multiplier
			break
		}
	}
	parsed, err := strconv.ParseInt(number, 10, 64)
	if err != nil || parsed <= 0 || parsed > math.MaxInt64/multiplier {
		return 0, configError(name + " must be a positive integer followed by B, KiB, MiB, or GiB")
	}
	return parsed * multiplier, nil
}

func configError(message string) error {
	return saferr.New(saferr.CategoryConfiguration, message)
}
