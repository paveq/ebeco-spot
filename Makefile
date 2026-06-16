# ebeco-spot — build and run helpers.
#
# Credentials come from the environment, never the config file:
#   export EBECO_EMAIL=you@example.com
#   export EBECO_PASSWORD=secret
#
# Override the config path with CONFIG=..., e.g. `make run CONFIG=/etc/ebeco.toml`.

BINARY  := bin/ebeco-spot
PKG     := ./cmd/ebeco-spot
CONFIG  ?= config.toml
GOFLAGS ?=

# Build static, dependency-free binaries (no libc linkage).
export CGO_ENABLED := 0

.DEFAULT_GOAL := build

.PHONY: build
build: ## Compile the binary to bin/ebeco-spot
	go build $(GOFLAGS) -o $(BINARY) $(PKG)

.PHONY: run
run: ## Run the controller (needs EBECO_EMAIL/EBECO_PASSWORD in the environment)
	go run $(PKG) -config $(CONFIG)

.PHONY: list
list: ## Authenticate, list devices (id, name, program) and exit
	go run $(PKG) -config $(CONFIG) -list

.PHONY: test
test: ## Run the test suite
	go test ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go sources in place
	gofmt -w .

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum
	go mod tidy

.PHONY: check
check: fmt vet test ## Format, vet and test

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'
