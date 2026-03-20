GO := /usr/local/go/bin/go

.PHONY: check lint test build

check: lint test

lint:
	$(GO) vet ./...

test:
	$(GO) test ./... -v

build:
	$(GO) build -o fixclaw .
