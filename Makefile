APP := opencode2api
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/6Kmfi6HP/opencode2api/internal/app.version=$(VERSION) -X github.com/6Kmfi6HP/opencode2api/internal/app.commit=$(COMMIT) -X github.com/6Kmfi6HP/opencode2api/internal/app.date=$(DATE)

.PHONY: fmt test vet build release-snapshot clean

fmt:
	gofmt -w ./cmd ./internal

test:
	go test ./...

vet:
	go vet ./...

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/opencode2api

release-snapshot:
	VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) ./scripts/build-release.sh

clean:
	rm -rf bin dist
