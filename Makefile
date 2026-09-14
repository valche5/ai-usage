BIN     := ai-usage
PREFIX  ?= $(HOME)/.local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build build-web install uninstall test vet fmt check clean release snapshot

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/ai-usage

build-web:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o ai-usage-web ./cmd/ai-usage-web

install: build
	mkdir -p $(PREFIX)/bin
	ln -sf $(CURDIR)/$(BIN) $(PREFIX)/bin/$(BIN)
	@echo "linked $(PREFIX)/bin/$(BIN) -> $(CURDIR)/$(BIN)"

uninstall:
	rm -f $(PREFIX)/bin/$(BIN)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: fmt vet test
	@test -z "$$(gofmt -l .)" || { echo "gofmt found issues"; gofmt -l .; exit 1; }

snapshot:
	goreleaser release --snapshot --clean

release:
	goreleaser release --clean

clean:
	rm -f $(BIN) ai-usage-web
	rm -rf dist
