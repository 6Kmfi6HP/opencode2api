# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# Needed only for the CA bundle copied into the scratch runtime image.
RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN set -eux; \
    if [ "$TARGETARCH" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -trimpath \
      -ldflags "-s -w -X github.com/6Kmfi6HP/opencode2api/internal/app.version=$VERSION -X github.com/6Kmfi6HP/opencode2api/internal/app.commit=$COMMIT -X github.com/6Kmfi6HP/opencode2api/internal/app.date=$DATE" \
      -o /out/opencode2api ./cmd/opencode2api

# Pre-create /data owned by the runtime UID so that `--user 65532:65532` works
# with a *named* volume (Docker seeds it from this directory). A bind mount keeps
# the host owner instead; pre-chown the host path or omit --user.
RUN mkdir -p /out/data

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/opencode2api /usr/local/bin/opencode2api
COPY config.example.json /app/config.example.json

WORKDIR /data

# The binary performs the work that used to live in docker/entrypoint.sh:
# initial config, /data ownership, and dropping root to 65532.
ENV OPENCODE2API_CONTAINER=1 \
    OPENCODE2API_PORT=8000 \
    OPENCODE2API_CONFIG=/data/config.json \
    OPENCODE2API_PASSWORD=123456 \
    OPENCODE2API_LOG_FILE=/data/opencode2api.log \
    HOME=/data \
    TMPDIR=/data

EXPOSE 8000

# No shell/wget in the image; the binary probes itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/usr/local/bin/opencode2api", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/opencode2api"]
