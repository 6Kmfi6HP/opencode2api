# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build

WORKDIR /src

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

COPY go.mod ./
RUN go mod download

COPY main.go main_test.go ./

RUN set -eux; \
    if [ "$TARGETARCH" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
      -o /out/opencode2api .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates su-exec tzdata wget \
    && addgroup -S opencode2api \
    && adduser -S -G opencode2api -h /data opencode2api \
    && mkdir -p /app /data \
    && chown -R opencode2api:opencode2api /data

WORKDIR /data

COPY --from=build /out/opencode2api /usr/local/bin/opencode2api
COPY config.example.json /app/config.example.json
COPY docker/entrypoint.sh /entrypoint.sh

ENV OPENCODE2API_PORT=8000 \
    OPENCODE2API_CONFIG=/data/config.json \
    OPENCODE2API_PASSWORD=123456

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${OPENCODE2API_PORT}/health" >/dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
