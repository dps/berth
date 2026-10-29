VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet install clean site

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test:
	go test -race ./...

vet:
	go vet ./...

# Installs next to each other: berth finds berthd in its own directory.
install: build
	install -m 0755 bin/berth bin/berthd /opt/homebrew/bin/

# The docs pages of berth.computer, from README.md (see tools/sitegen).
site:
	cd tools/sitegen && go run . -readme ../../README.md -out ../../site/public
	cp docs/icon.svg site/public/assets/icon.svg

clean:
	rm -rf bin
