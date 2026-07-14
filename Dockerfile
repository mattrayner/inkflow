# syntax=docker/dockerfile:1

# ── Stage 1: Build ────────────────────────────────────────────────────────────
# $BUILDPLATFORM pins the builder to the runner's native arch (amd64 on GitHub
# Actions) so Go always cross-compiles rather than running under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build

# Cache deps separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build-time metadata injected via ldflags
ARG VERSION=dev
ARG COMMIT=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT=

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM="${TARGETVARIANT#v}" \
    go build \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o inkflow \
      ./cmd/inkflow

# ── Stage 2: Runtime ──────────────────────────────────────────────────────────
# distroless/static includes CA certs and tzdata; no shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /build/inkflow /app/inkflow

# Config is expected to be mounted at /config/inkflow.toml
# Vault and state dirs should be mounted as volumes (see below)
WORKDIR /app

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/app/inkflow"]
CMD ["serve", "--config", "/config/inkflow.toml"]
