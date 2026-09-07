# syntax=docker/dockerfile:1

# Builder: modernc.org/sqlite is a pure-Go SQLite driver (no cgo), so
# CGO_ENABLED=0 produces a fully static binary and the runtime stage below
# can be gcr.io/distroless/static-debian12 (no libc needed). See
# docs/operations/configuration.md for why this deployment always runs
# with foreign_keys/journal_mode fixed rather than operator-configurable.
FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
# /go/pkg/mod (GOPATH/pkg/mod, GOPATH=/go in this base image) and
# /root/.cache/go-build (GOCACHE, HOME=/root as builder runs as root) are
# BuildKit cache mounts, not image layers: their contents persist across
# builds (docker-publish.yml's cache-to: type=gha,mode=max exports them
# alongside the ordinary layer cache) without ever being baked into this
# stage's own filesystem — so, unlike the COPY layers above and below,
# they keep working even though COPY . . invalidates on nearly every
# commit. Every RUN below mounts the same /go/pkg/mod so `go build` sees
# exactly what `go mod download` just populated (Issue #92).
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/miauthctl ./cmd/miauthctl

# Runtime: distroless static + nonroot (uid 65532). No shell, so /data
# must already be writable by that uid when the container starts — see
# the README's Container section. ca-certificates is included in this
# base image for other HTTPS integrations.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/server /server
COPY --from=builder /out/miauthctl /miauthctl

# DB_PATH is set to an absolute path rather than config.Load's relative
# "./data/portal.db" default so it does not depend on the runtime's
# working directory. HTTP_HOST/HTTP_PORT restate config.Load's own
# defaults explicitly, since a container's networking makes them worth
# stating rather than leaving implicit.
ENV DB_PATH=/data/portal.db \
    HTTP_HOST=0.0.0.0 \
    HTTP_PORT=8080

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/server"]
