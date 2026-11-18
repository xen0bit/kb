BIN := kb
VERSION ?= 0.1.0-dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG := github.com/xen0bit/kb/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
SHELL := /bin/bash

.PHONY: all
all: build

# A plain build serves web/dist from disk when run in a checkout, and a
# minimal fallback stylesheet otherwise. build-with-web embeds the Bun-built
# assets, which is what releases do.
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/kb

.PHONY: build-with-web
build-with-web: web.build
	CGO_ENABLED=0 go build -trimpath -tags embed_web -ldflags="$(LDFLAGS)" -o $(BIN) ./cmd/kb

.PHONY: install
install: web.build
	CGO_ENABLED=0 go install -trimpath -tags embed_web -ldflags="$(LDFLAGS)" ./cmd/kb

.PHONY: test
test:
	go test -race ./...
	@$(MAKE) --no-print-directory web.test

.PHONY: test-short
test-short:
	go test ./...

# embed_web needs web/dist to exist, so lint builds the assets first.
.PHONY: lint
lint: web.build
	go vet ./...
	go vet -tags embed_web ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./... && golangci-lint run --build-tags embed_web ./...; \
	else echo "golangci-lint not found, skipping"; fi

.PHONY: fmt
fmt:
	gofmt -w cmd internal web/*.go

.PHONY: web.build
web.build:
	@command -v bun >/dev/null 2>&1 || { echo "bun is required: https://bun.sh"; exit 1; }
	mkdir -p web/dist
	cd web/src && bun run build
	@test -s web/dist/app.css && test -s web/dist/app.js || { echo "web build did not produce app.css and app.js"; exit 1; }

.PHONY: web.test
web.test:
	@command -v bun >/dev/null 2>&1 || { echo "bun is required: https://bun.sh"; exit 1; }
	cd web/src && bun test

# Serve a scratch kb with assets read from disk, for working on templates and
# web/src (re-run make web.build after changing web/src).
.PHONY: dev
dev: build web.build
	./$(BIN) serve -db ./dev.db -assets web/dist -mcp

.PHONY: snapshot
snapshot:
	goreleaser release --snapshot --clean --skip=sign,publish,announce

.PHONY: clean
clean:
	go clean ./...
	$(RM) $(BIN) dev.db dev.db-shm dev.db-wal coverage.out
	$(RM) -r dist web/dist

.PHONY: version
version:
	@echo "$(VERSION) ($(COMMIT))"

.PHONY: help
help:
	@echo "make build           build ./kb (assets from disk or fallback)"
	@echo "make build-with-web  build ./kb with the Bun-built viewer assets embedded"
	@echo "make install         go install with embedded assets"
	@echo "make test            Go tests (-race) and the viewer's bun tests"
	@echo "make lint            go vet + golangci-lint, both build tag sets"
	@echo "make web.build       build web/src into web/dist with bun"
	@echo "make dev             serve ./dev.db with live assets and MCP at /mcp"
	@echo "make snapshot        goreleaser dry run into dist/"
	@echo "make clean"
