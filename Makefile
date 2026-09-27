BIN     := bin/uplink
PKG     := ./cmd/uplink
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: build test race vet fmt cross clean install

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

test:
	go test -race ./...

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

fmt:
	gofmt -w .

# Crew binaries for the usual devbox targets. No cgo, no dependencies, so these
# are a single scp away from running anywhere.
cross:
	@mkdir -p dist
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/uplink-linux-amd64  $(PKG)
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/uplink-linux-arm64  $(PKG)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/uplink-darwin-arm64 $(PKG)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/uplink-darwin-amd64 $(PKG)
	@ls -lh dist/

install: build
	install -m 0755 $(BIN) $(HOME)/.local/bin/uplink

clean:
	rm -rf bin dist
