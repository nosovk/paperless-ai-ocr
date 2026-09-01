# Capability Probe Fixture Clipping Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Release v0.1.3 with capability-probe fixtures whose complete visual nonce is readable by the configured model.

**Architecture:** Keep the strict Responses API envelope and exact nonce validation unchanged. Replace only the clipped synthetic PDF/PNG pair, and add a fixture-level regression test requiring visible padding on every side of the rendered content.

**Tech Stack:** Go standard library image decoding, embedded PNG/PDF fixtures, ImageMagick/Poppler fixture generation, GitHub Actions release workflow.

---

### Task 1: Reproduce Fixture Clipping

**Files:**
- Modify: `internal/aigate/capability_test.go`

1. Add a test that decodes the embedded PNG and requires non-background content to have visible padding on all four sides.
2. Run `go test ./internal/aigate -run 'TestProbeFixtures'` and require it to fail because the current content reaches both horizontal edges.

### Task 2: Replace Synthetic Fixtures

**Files:**
- Modify: `testdata/probe/capability.png`
- Modify: `testdata/probe/capability.pdf`

1. Generate a wider, padded image containing exactly `OCR-PROBE-7K3M9Q2X`.
2. Generate the one-page PDF from the same image.
3. Run the focused fixture test and require it to pass.
4. Probe both transports against AI Gate and require exact nonce matches without printing credentials or full provider payloads.

### Task 3: Verify And Release

**Files:**
- Modify: `CHANGELOG.md`

1. Record the fixture clipping fix under v0.1.3.
2. Run focused tests, `go test ./...`, `go test -race ./...`, `go vet ./...`, `git diff --check`, and markdown lint.
3. Commit and push without rewriting v0.1.2, tag v0.1.3, and verify the immutable multi-architecture release.

### Task 4: Deploy And Harden

**Files:**
- Modify the Paperless stack digest, tests, and deployment documentation in the Homelab worktree.

1. Update Homelab to the v0.1.3 index digest and verify configuration.
2. Commit/push and allow Doco-CD to deploy without restarting Docker during active transfer.
3. Require all seven containers healthy, then verify runtime hardening and disposable smoke cases.
4. Keep workflow ID 3 disabled until every disposable smoke case succeeds.
