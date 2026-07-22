BIN     := cablecheck
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

export CGO_ENABLED=0

.PHONY: build test test-race vet fmt fmt-check lint tidy-check dist examples demo-e2e clean check verify

build:
	go build -trimpath $(LDFLAGS) -o $(BIN) ./cmd/cablecheck

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race -shuffle=on ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

# staticcheck is optional on dev/CI machines: run when present, never required.
lint: vet fmt-check
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed (skipped)"

tidy-check:
	go mod tidy -diff

dist:
	GOOS=linux GOARCH=amd64 go build -trimpath $(LDFLAGS) -o dist/$(BIN)-linux-amd64 ./cmd/cablecheck
	GOOS=linux GOARCH=arm64 go build -trimpath $(LDFLAGS) -o dist/$(BIN)-linux-arm64 ./cmd/cablecheck

examples:
	go run ./tools/genexamples -out examples

demo-e2e: build
	./scripts/demo-e2e.sh

clean:
	rm -rf $(BIN) dist/ cablecheck-report-*

check: fmt-check vet tidy-check test test-race build

verify: check dist examples demo-e2e
