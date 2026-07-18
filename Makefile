BIN := bridge
PKG := ./cmd/bridge
# VERSION: a pushed git tag (CI), else a sha-suffixed dev string (CI on a
# branch), else `git describe` (local), else `dev`. In GitHub Actions on a
# tag push, GITHUB_REF_TYPE=tag and GITHUB_REF_NAME is the tag (e.g. v1.2.3);
# on a branch push GITHUB_SHA is the commit, used here for a semver-friendly
# dev fallback because actions/checkout's shallow clone has no tags.
# `?=` so an explicit `VERSION=foo make ...` still wins.
ifeq ($(GITHUB_REF_TYPE),tag)
  VERSION ?= $(GITHUB_REF_NAME)
else ifneq ($(GITHUB_SHA),)
  VERSION ?= 0.0.0-dev+$(shell echo $(GITHUB_SHA) | cut -c1-7)
else
  VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
endif
LDFLAGS := -s -w -X bridge/internal/config.DefaultVersion=$(VERSION)

# Install location. The service keeps config.json, index.txt and the logs/
# folder next to the executable, so a stable directory is important.
# Override with `make install INSTALL_DIR=/some/path`.
ifeq ($(OS),Windows_NT)
  EXT := .exe
  INSTALL_DIR ?= C:/opt/bridge
else
  EXT :=
  INSTALL_DIR ?= /opt/bridge
endif
BIN_OUT := $(BIN)$(EXT)

.PHONY: build build-all package install uninstall clean tidy fmt print-version

# print-version is what the release workflow shells out to so version logic
# lives in exactly one place (this file) instead of also in release.yml.
print-version:
	@echo $(VERSION)

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_OUT) $(PKG)

# Build and place the binary in INSTALL_DIR (e.g. /opt/bridge). Creating
# /opt typically needs root: `sudo make install`.
install: build
	mkdir -p "$(INSTALL_DIR)"
	cp "$(BIN_OUT)" "$(INSTALL_DIR)/$(BIN_OUT)"
	@echo "Installed $(BIN_OUT) to $(INSTALL_DIR)"
	@echo "Next: cd $(INSTALL_DIR) && sudo ./$(BIN_OUT) pair <code> --api-url <url> && sudo ./$(BIN_OUT) install"

uninstall:
	rm -f "$(INSTALL_DIR)/$(BIN_OUT)"
	@echo "Removed $(INSTALL_DIR)/$(BIN_OUT) (config.json, index.txt and logs/ in $(INSTALL_DIR) are left intact)"

build-all:
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64        go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)_$(VERSION)_darwin_amd64        $(PKG)
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64        go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)_$(VERSION)_darwin_arm64        $(PKG)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64        go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)_$(VERSION)_linux_amd64         $(PKG)
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64        go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)_$(VERSION)_linux_arm64         $(PKG)
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm GOARM=7  go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)_$(VERSION)_linux_armv7         $(PKG)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64        go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)_$(VERSION)_windows_amd64.exe   $(PKG)
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64        go build -trimpath -ldflags "$(LDFLAGS)" -o dist/$(BIN)_$(VERSION)_windows_arm64.exe   $(PKG)

# package tars up the cross-built binaries from build-all alongside the
# per-OS launchers (Linux .desktop; macOS ships the bare binary because the
# .app bundle path was abandoned — pure-Go binaries don't satisfy
# LaunchServices' UI-runloop liveness check, and adding the detach +
# elevation hand-off was more complexity than running `sudo ./bridge`
# from terminal). Windows is covered by build-all alone (the committed
# resource_windows_*.syso embeds the requireAdministrator manifest).
package: build-all
	@for arch in amd64 arm64; do \
	  (cd dist && tar -czf "$(BIN)_$(VERSION)_darwin_$$arch.tar.gz" "$(BIN)_$(VERSION)_darwin_$$arch"); \
	  echo "packaged dist/$(BIN)_$(VERSION)_darwin_$$arch.tar.gz"; \
	done
	@for arch in amd64 arm64 armv7; do \
	  cp packaging/linux/bridge.desktop dist/bridge.desktop; \
	  (cd dist && tar -czf "$(BIN)_$(VERSION)_linux_$$arch.tar.gz" "$(BIN)_$(VERSION)_linux_$$arch" bridge.desktop); \
	  rm -f dist/bridge.desktop; \
	  echo "packaged dist/$(BIN)_$(VERSION)_linux_$$arch.tar.gz"; \
	done

tidy:
	go mod tidy

fmt:
	gofmt -w .

clean:
	rm -rf $(BIN) $(BIN_OUT) dist
