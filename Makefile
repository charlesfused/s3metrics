BINARY  := s3metrics
PKG     := github.com/charlesfused/s3metrics
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/buildinfo.Version=$(VERSION) \
	-X $(PKG)/internal/buildinfo.Commit=$(COMMIT) \
	-X $(PKG)/internal/buildinfo.Date=$(DATE)

.PHONY: build test race vet lint snapshot clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/s3metrics

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

lint: vet

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf $(BINARY) dist/
