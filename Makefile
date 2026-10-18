GO ?= $(shell command -v go 2>/dev/null || echo /opt/homebrew/bin/go)
BIN := bin/herdr-drawer

.PHONY: build test vet clean link

build:
	@mkdir -p bin
	$(GO) build -o $(BIN) .

test:
	$(GO) test -v ./...

vet:
	$(GO) vet ./...

link: build
	herdr plugin link "$(CURDIR)" --enabled

clean:
	rm -rf bin
