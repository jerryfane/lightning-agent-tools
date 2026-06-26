.PHONY: build unit test mod-tidy clean help

.DEFAULT_GOAL := build

PKG := github.com/lightninglabs/lightning-agent-kit/node-ops-daemon

GOTEST  := GO111MODULE=on go test -v
GOBUILD := GO111MODULE=on go build -v
GOMOD   := GO111MODULE=on go mod
GOLIST  := go list $(PKG)/... | grep -v '/vendor/'

GREEN := "\\033[0;32m"
NC    := "\\033[0m"
define print
	echo $(GREEN)$1$(NC)
endef

build: #? Build the node-ops-daemon binary
	@$(call print, "Building node-ops-daemon.")
	$(GOBUILD) -o node-ops-daemon .

test: unit #? Alias for unit tests

unit: #? Run unit tests
	@$(call print, "Running unit tests.")
	$(GOLIST) | xargs -L 1 env $(GOTEST) -timeout 60s

mod-tidy: #? Tidy Go modules
	@$(call print, "Tidying modules.")
	$(GOMOD) tidy

clean: #? Remove build artifacts
	@$(call print, "Cleaning.")
	rm -f node-ops-daemon

help: #? Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?#\? .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?#\\? "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
