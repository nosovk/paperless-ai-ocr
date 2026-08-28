package saferr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrorFormattingIsSafe(t *testing.T) {
	const providerBody = `{"error":"bad request","api_key":"canary-provider-secret"}`
	cause := errors.New(providerBody)
	err := Wrap(CategoryProvider, "model request failed", cause)

	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		t.Run(format, func(t *testing.T) {
			formatted := fmt.Sprintf(format, err)
			if got, want := formatted, "provider: model request failed"; got != want {
				t.Errorf("formatted error = %q, want %q", got, want)
			}
			if strings.Contains(formatted, providerBody) || strings.Contains(formatted, "canary-provider-secret") {
				t.Errorf("formatted error disclosed provider body: %q", formatted)
			}
		})
	}

	wrapped := fmt.Errorf("transcription: %w", err)
	for _, format := range []string{"%s", "%v", "%+v"} {
		t.Run("wrapped_"+format, func(t *testing.T) {
			formatted := fmt.Sprintf(format, wrapped)
			if got, want := formatted, "transcription: provider: model request failed"; got != want {
				t.Errorf("formatted wrapped error = %q, want %q", got, want)
			}
			if strings.Contains(formatted, providerBody) || strings.Contains(formatted, "canary-provider-secret") {
				t.Errorf("formatted wrapped error disclosed provider body: %q", formatted)
			}
		})
	}
	formattedWrappedGoSyntax := fmt.Sprintf("%#v", wrapped)
	if !strings.Contains(formattedWrappedGoSyntax, "transcription: provider: model request failed") {
		t.Errorf("formatted wrapped error = %q, want safe operator-facing message", formattedWrappedGoSyntax)
	}
	if strings.Contains(formattedWrappedGoSyntax, providerBody) || strings.Contains(formattedWrappedGoSyntax, "canary-provider-secret") {
		t.Errorf("formatted wrapped error disclosed provider body: %q", formattedWrappedGoSyntax)
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is(error, cause) = false, want true")
	}
}

func TestErrorCategory(t *testing.T) {
	tests := []Category{
		CategoryConfiguration,
		CategoryPaperless,
		CategoryProvider,
		CategoryValidation,
		CategoryRendering,
		CategoryInternal,
	}

	for _, category := range tests {
		t.Run(string(category), func(t *testing.T) {
			err := New(category, "safe message")
			safeError, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("errors.AsType[*Error](error) = false")
			}
			if safeError.Category() != category {
				t.Errorf("Category() = %q, want %q", safeError.Category(), category)
			}
			if got, want := err.Error(), string(category)+": safe message"; got != want {
				t.Errorf("Error() = %q, want %q", got, want)
			}
		})
	}
}

func TestConfigurationErrorDoesNotDiscloseSecret(t *testing.T) {
	const secret = "canary-config-secret"
	cause := fmt.Errorf("invalid API key %q", secret)
	err := Wrap(CategoryConfiguration, "AI_API_KEY is invalid", cause)

	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		formatted := fmt.Sprintf(format, err)
		if strings.Contains(formatted, secret) {
			t.Errorf("format %s disclosed secret: %q", format, formatted)
		}
	}
}
