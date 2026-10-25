VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet install clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test:
	go test -race ./...

vet:
	go vet ./...

# Installs next to each other: berth finds berthd in its own directory.
install: build
	install -m 0755 bin/berth bin/berthd /opt/homebrew/bin/

clean:
	rm -rf bin
