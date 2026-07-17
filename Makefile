.PHONY: build build-all test vet lint clean docker docker-poder docker-toolbox docker-toolbox-centos help

# ─── Go binaries ────────────────────────────────────────────────────────────

build: ## Build all binaries for the current platform
	go build -o server  ./cmd/server
	go build -o poder   ./cmd/poder
	go build -o agent   ./cmd/agent
	go build -o toolbox ./cmd/toolbox

# Stamp build version into the agent so audit events carry an identifying string.
# Override via VERSION=v1.2.3 make build-all; default is dirty git short-sha.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS_AGENT := -s -w -X main.agentVersion=$(VERSION)
LDFLAGS_BARE  := -s -w

build-all: ## Cross-compile release binaries into dist/
	mkdir -p dist
	# server (Linux only — runs on the control-plane host)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="$(LDFLAGS_BARE)"  -o dist/server-linux-amd64                  ./cmd/server
	# sandrpod-agent — runs on every employee PC (CGO off; pure-Go works on every target)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="$(LDFLAGS_AGENT)" -o dist/sandrpod-agent-linux-amd64          ./cmd/agent
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -ldflags="$(LDFLAGS_AGENT)" -o dist/sandrpod-agent-linux-arm64          ./cmd/agent
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -ldflags="$(LDFLAGS_AGENT)" -o dist/sandrpod-agent-darwin-amd64         ./cmd/agent
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags="$(LDFLAGS_AGENT)" -o dist/sandrpod-agent-darwin-arm64         ./cmd/agent
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS_AGENT)" -o dist/sandrpod-agent-windows-amd64.exe    ./cmd/agent
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags="$(LDFLAGS_AGENT)" -o dist/sandrpod-agent-windows-arm64.exe    ./cmd/agent
	# sandrpod-tray needs CGO (systray uses native Cocoa / win32 / GTK APIs).
	#
	# We can cross-compile to:
	#   - host platform (always)
	#   - Windows amd64 from any host with mingw-w64 installed
	#     (`brew install mingw-w64` on macOS / `apt install mingw-w64` on Linux)
	#
	# We currently CANNOT cross-compile to Linux from a non-Linux host without
	# a much heavier toolchain (libgtk-3-dev cross + libappindicator headers).
	# Linux tray ships when CI grows a Linux job; until then the Linux install
	# path uses the agent's in-process zenity/kdialog prompter (no menubar
	# icon, but consent dialogs work).
	@echo "→ building tray for host ($$(go env GOOS)/$$(go env GOARCH))"
	go build -ldflags="$(LDFLAGS_BARE)" -o dist/sandrpod-tray-$$(go env GOOS)-$$(go env GOARCH) ./cmd/sandrpod-tray
	@if command -v x86_64-w64-mingw32-gcc >/dev/null 2>&1; then \
		echo "→ building tray for windows/amd64 via mingw-w64"; \
		CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
			CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ \
			go build -ldflags="$(LDFLAGS_BARE) -H windowsgui" \
			-o dist/sandrpod-tray-windows-amd64.exe ./cmd/sandrpod-tray; \
	else \
		echo "→ skipping windows/amd64 tray (no mingw-w64; \`brew install mingw-w64\`)"; \
	fi
	@$(MAKE) tray-linux

# tray-linux uses Docker to build the Linux tray inside a real Linux
# environment (avoids dragging GTK + appindicator dev headers onto the build
# host). Requires Docker; silently skipped if Docker isn't installed.
#
# Both amd64 and arm64 are built. On an Apple Silicon Mac the arm64 build is
# native speed (Docker Desktop's Linux VM is arm64); the amd64 build runs
# under QEMU user-mode emulation and is ~3-4× slower. On an Intel Mac it's
# the inverse. apt-get + go build for one arch takes 1-2 min native, 4-6 min
# under emulation — still much faster than setting up a cross-toolchain.
.PHONY: tray-linux tray-linux-amd64 tray-linux-arm64
tray-linux: tray-linux-amd64 tray-linux-arm64

tray-linux-amd64:
	@$(MAKE) _docker_tray DOCKER_PLATFORM=linux/amd64 OUT=sandrpod-tray-linux-amd64

tray-linux-arm64:
	@$(MAKE) _docker_tray DOCKER_PLATFORM=linux/arm64 OUT=sandrpod-tray-linux-arm64

# Internal target: build the Linux tray inside a Debian/Go container of the
# requested arch. Factored out so amd64 and arm64 share the recipe.
.PHONY: _docker_tray
_docker_tray:
	@if ! command -v docker >/dev/null 2>&1; then \
		echo "→ skipping $(OUT) (Docker not installed)"; \
		exit 0; \
	fi
	@echo "→ building $(OUT) via Docker ($(DOCKER_PLATFORM))"
	@mkdir -p dist
	@docker run --rm --platform $(DOCKER_PLATFORM) \
		-v "$$PWD":/src -w /src \
		golang:1.25 \
		bash -c 'apt-get update -qq >/dev/null 2>&1 && \
		         apt-get install -y -qq libgtk-3-dev libayatana-appindicator3-dev pkg-config gcc >/dev/null 2>&1 && \
		         CGO_ENABLED=1 go build -ldflags="$(LDFLAGS_BARE)" \
		           -o dist/$(OUT) ./cmd/sandrpod-tray'

# ─── Tests ──────────────────────────────────────────────────────────────────

test: ## Run all tests with race detector
	go test -race -timeout 120s ./...

vet: ## Run go vet
	go vet ./...

lint: vet ## Run vet + staticcheck (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck ./...

# ─── Docker images ──────────────────────────────────────────────────────────

docker-poder: ## Build the Poder Docker image (linux/amd64)
	docker buildx build --platform linux/amd64 \
		-f docker/Dockerfile.poder \
		-t ghcr.io/sandrpod/poder:latest --load .

docker-toolbox: ## Build the Toolbox Docker image - Alpine (linux/amd64)
	docker buildx build --platform linux/amd64 \
		-f docker/Dockerfile.toolbox \
		-t ghcr.io/sandrpod/toolbox:latest --load .

docker-toolbox-centos: ## Build the Toolbox Docker image - CentOS Stream 9 full-featured (linux/amd64)
	docker buildx build --platform linux/amd64 \
		-f docker/Dockerfile.toolbox.centos \
		-t ghcr.io/sandrpod/toolbox:centos --load .

docker: docker-poder docker-toolbox ## Build all Docker images

# ─── Maintenance ────────────────────────────────────────────────────────────

clean: ## Remove built binaries
	rm -f server poder agent toolbox
	rm -rf dist/

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
