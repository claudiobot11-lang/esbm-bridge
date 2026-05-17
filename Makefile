# Build targets for esbm-bridge.
#
# `make build`       -> native binary in ./bin (for local dev/test)
# `make windows`     -> bin/esbm-bridge.exe (amd64, what we ship)
# `make release`     -> windows + linux + darwin in dist/
# `make test`        -> unit tests
# `make tidy`        -> go mod tidy
# `make clean`       -> remove ./bin and ./dist

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o bin/esbm-bridge ./cmd/bridge

.PHONY: windows
windows:
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/esbm-bridge.exe ./cmd/bridge

.PHONY: release
release: clean
	mkdir -p dist
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS) -s -w" -o dist/esbm-bridge-windows-amd64.exe ./cmd/bridge
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS) -s -w" -o dist/esbm-bridge-linux-amd64       ./cmd/bridge
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS) -s -w" -o dist/esbm-bridge-darwin-arm64      ./cmd/bridge

.PHONY: test
test:
	go test ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: clean
clean:
	rm -rf bin dist
