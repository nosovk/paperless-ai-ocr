# syntax=docker/dockerfile:1.20

FROM golang:1.26.6-alpine3.23@sha256:e57c41c1d5864341031181b0db34b9a537bb5773eb6428e4e5bdaea0f9135406 AS build

ARG VERSION=development
ARG REVISION=unknown
ARG CREATED=unknown

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY testdata ./testdata

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.version=${VERSION} -X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.revision=${REVISION} -X github.com/nosovk/paperless-ai-ocr/internal/buildinfo.buildTime=${CREATED}" \
    -o /out/paperless-ai-ocr \
    ./cmd/paperless-ai-ocr

FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659

ARG VERSION=development
ARG REVISION=unknown
ARG CREATED=unknown

LABEL org.opencontainers.image.source="https://github.com/nosovk/paperless-ai-ocr" \
      org.opencontainers.image.title="paperless-ai-ocr" \
      org.opencontainers.image.description="Validated multimodal AI transcription for Paperless-ngx" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}"

RUN apk add --no-cache \
        ca-certificates \
        poppler-utils \
    && addgroup -S -g 65532 paperless-ai-ocr \
    && adduser -S -D -H -u 65532 -G paperless-ai-ocr paperless-ai-ocr \
    && install -d -o paperless-ai-ocr -g paperless-ai-ocr -m 0700 /app/data \
    && printf '%s\n' \
        '#!/bin/sh' \
        'exec /usr/bin/wget --quiet --tries=1 --timeout=2 --spider "http://127.0.0.1:${HTTP_PORT:-8080}/health"' \
        > /usr/local/bin/healthcheck \
    && chmod 0755 /usr/local/bin/healthcheck

WORKDIR /app

COPY --from=build --chown=65532:65532 /out/paperless-ai-ocr /usr/local/bin/paperless-ai-ocr

USER 65532:65532
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/healthcheck"]

ENTRYPOINT ["/usr/local/bin/paperless-ai-ocr"]
