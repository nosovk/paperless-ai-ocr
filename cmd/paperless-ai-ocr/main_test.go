package main_test

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

func buildCommand(t *testing.T, linkerFlags string) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "paperless-ai-ocr")
	args := []string{"build", "-o", binary}
	if linkerFlags != "" {
		args = append(args, "-ldflags", linkerFlags)
	}
	args = append(args, ".")

	if output, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("build command: %v\n%s", err, output)
	}
	return binary
}

func TestDevelopmentCommandBehavior(t *testing.T) {
	binary := buildCommand(t, "")

	t.Run("version", func(t *testing.T) {
		cmd := exec.Command(binary, "--version")
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			t.Fatalf("command returned an error: %v", err)
		}
		if got, want := stdout.String(), "paperless-ai-ocr version=development revision=unknown build_time=unknown\n"; got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
		if got := stderr.String(); got != "" {
			t.Errorf("stderr = %q, want empty", got)
		}
	})

	t.Run("invalid configuration", func(t *testing.T) {
		cmd := exec.Command(binary)
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		err := cmd.Run()
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
			t.Fatalf("command error = %v, want nonzero exit", err)
		}
		if got := stdout.String(); got != "" {
			t.Errorf("stdout = %q, want empty", got)
		}
		if got, want := stderr.String(), "paperless-ai-ocr: startup failed\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})
}

func TestLinkerInjectedVersion(t *testing.T) {
	const linkerFlags = "-X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.version=v1.2.3 " +
		"-X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.revision=abc123 " +
		"-X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.buildTime=2026-08-29T12:00:00Z"
	binary := buildCommand(t, linkerFlags)

	output, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("command returned an error: %v", err)
	}
	if got, want := string(output), "paperless-ai-ocr version=v1.2.3 revision=abc123 build_time=2026-08-29T12:00:00Z\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}
