.PHONY: build run test lint clean docker docker-up docker-down tidy setup install-hooks

BINARY := meshtastic-proxy
BUILD_DIR := ./bin

# Auto-detect Go 1.23+ binary
GO := $(shell \
	if go version 2>/dev/null | grep -qE 'go1\.(2[3-9]|[3-9][0-9])'; then echo go; \
	elif command -v go1.23.0 >/dev/null 2>&1; then echo go1.23.0; \
	elif [ -x "$(HOME)/sdk/go1.23.0/bin/go" ]; then echo "$(HOME)/sdk/go1.23.0/bin/go"; \
	elif [ -x "$(HOME)/go/bin/go1.23.0" ]; then echo "$(HOME)/go/bin/go1.23.0"; \
	else echo go; \
	fi)

# Auto-detect golangci-lint binary
GOLANGCI_LINT := $(shell \
	if command -v golangci-lint >/dev/null 2>&1; then echo golangci-lint; \
	elif [ -x "$(HOME)/go/bin/golangci-lint" ]; then echo "$(HOME)/go/bin/golangci-lint"; \
	fi)

build:
	$(GO) build -o $(BUILD_DIR)/$(BINARY) ./cmd/meshtastic-proxy

run: build
	$(BUILD_DIR)/$(BINARY) -config config.toml

test:
	$(GO) test -v -race ./...

lint:
	$(GO) vet ./...
ifdef GOLANGCI_LINT
	GOROOT=$(shell $(GO) env GOROOT) PATH=$(shell dirname $(shell which $(GO) 2>/dev/null || echo $(GO))):$$PATH $(GOLANGCI_LINT) run --timeout=5m
else
	@echo "golangci-lint not installed, skipping (https://golangci-lint.run/usage/install/)"
endif

clean:
	rm -rf $(BUILD_DIR)

docker:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down

tidy:
	$(GO) mod tidy

# Install Go 1.23.0 if not available
setup:
	@if ! $(GO) version 2>/dev/null | grep -qE 'go1\.(2[3-9]|[3-9][0-9])'; then \
		echo "Go 1.23+ not found. Installing go1.23.0..."; \
		GOTOOLCHAIN=local go install golang.org/dl/go1.23.0@latest; \
		$(HOME)/go/bin/go1.23.0 download; \
		echo "Done. go1.23.0 installed to $(HOME)/sdk/go1.23.0"; \
		echo "Run 'make build' to build the project."; \
	else \
		echo "Go 1.23+ already available: $$($(GO) version)"; \
	fi

# Install git pre-commit hook (runs vet + test + lint before each commit)
install-hooks:
	git config core.hooksPath .githooks
	@echo "Git hooks installed from .githooks/"
