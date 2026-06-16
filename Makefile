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

# macOS LaunchAgent install layout (see `make install`). Override PREFIX to
# install elsewhere; the binary and config.toml live there together.
LABEL   := com.github.paveq.ebeco-spot
PREFIX  ?= $(HOME)/.local/share/ebeco-spot
PLIST   := $(HOME)/Library/LaunchAgents/$(LABEL).plist
DOMAIN  := gui/$(shell id -u)

# On macOS, cgo is required for the os_log (unified logging) backend and
# libSystem is always present, so enable it. Elsewhere build static,
# dependency-free binaries (no libc linkage).
ifeq ($(shell uname -s),Darwin)
export CGO_ENABLED := 1
else
export CGO_ENABLED := 0
endif

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

.PHONY: install
install: build ## Install & start the macOS LaunchAgent (per-user, reads Keychain)
	@command -v launchctl >/dev/null || { echo "install target is macOS-only"; exit 1; }
	@mkdir -p "$(PREFIX)" "$(dir $(PLIST))"
	install -m 0755 $(BINARY) "$(PREFIX)/ebeco-spot"
	@if [ ! -f "$(PREFIX)/config.toml" ]; then \
		install -m 0644 $(CONFIG) "$(PREFIX)/config.toml"; \
		echo "installed config to $(PREFIX)/config.toml — edit device_ids there"; \
	else \
		echo "kept existing $(PREFIX)/config.toml"; \
	fi
	sed -e 's|__LABEL__|$(LABEL)|g' \
	    -e 's|__PREFIX__|$(PREFIX)|g' \
	    dist/com.github.paveq.ebeco-spot.plist.in > "$(PLIST)"
	@launchctl bootout $(DOMAIN) "$(PLIST)" 2>/dev/null || true
	launchctl enable $(DOMAIN)/$(LABEL)
	launchctl bootstrap $(DOMAIN) "$(PLIST)"
	@echo "installed and started (RunAtLoad); logs: make logs"

.PHONY: uninstall
uninstall: ## Stop & remove the macOS LaunchAgent (leaves installed files)
	@launchctl bootout $(DOMAIN) "$(PLIST)" 2>/dev/null || true
	rm -f "$(PLIST)"
	@echo "uninstalled; files under $(PREFIX) left in place"

.PHONY: status
status: ## Show the LaunchAgent's state
	@launchctl print $(DOMAIN)/$(LABEL) 2>/dev/null || echo "not loaded (run: make install)"

.PHONY: logs
logs: ## Stream logs from the unified logging system (Ctrl-C to stop)
	@log stream --predicate 'subsystem == "com.github.paveq.ebeco-spot"' --level info

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
