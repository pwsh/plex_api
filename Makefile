# plex-api -- build and container packaging
#
# The Go service is built CGO-free and static so it can be dropped into the
# linuxserver/plex container (or shipped as a FROM scratch docker mod).

BINARY      := plex-api
PKG         := ./cmd/plex-api
BIN_DIR     := bin
BIN         := $(BIN_DIR)/$(BINARY)

MOD_IMAGE   := plex-api-mod:dev
LAYER_IMAGE := plex-api-layer:dev

GO          ?= go
DOCKER      ?= docker

GOFLAGS_BUILD := -trimpath -ldflags='-s -w'

.PHONY: all build test mod layer images smoke clean help

all: build

## build: compile the static binary to bin/plex-api
build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS_BUILD) -o $(BIN) $(PKG)
	@ls -lh $(BIN)

## test: run the Go test suite
test:
	$(GO) test ./...

## mod: build the FROM scratch linuxserver docker-mod image
mod:
	$(DOCKER) build -f deploy/Dockerfile.mod -t $(MOD_IMAGE) .

## layer: build linuxserver/plex with plex-api baked in
layer:
	$(DOCKER) build -f deploy/Dockerfile.layer -t $(LAYER_IMAGE) .

## images: build both container images
images: mod layer

## smoke: run the container integration smoke test (needs the real test DBs)
smoke:
	./deploy/test-container.sh

## clean: remove build output
clean:
	rm -rf $(BIN_DIR)
	$(GO) clean -cache -testcache 2>/dev/null || true

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
