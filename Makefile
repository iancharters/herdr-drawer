GO ?= $(shell command -v go 2>/dev/null || echo /opt/homebrew/bin/go)
BIN := bin/herdr-drawer

.PHONY: build test vet clean link setup

build:
	@mkdir -p bin
	$(GO) build -ldflags="-s -w" -o $(BIN) .

test:
	$(GO) test -v ./...

vet:
	$(GO) vet ./...

link: build
	herdr plugin link "$(CURDIR)" --enabled

setup: build
	./$(BIN) setup

clean:
	rm -rf bin dist
