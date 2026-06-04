APP := opencode2api
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: fmt test vet build release-snapshot clean

fmt:
	gofmt -w main.go main_test.go

test:
	go test ./...

vet:
	go vet ./...

build:
	mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) .

release-snapshot:
	VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) ./scripts/build-release.sh

clean:
	rm -rf bin dist
