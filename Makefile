GO ?= go

.PHONY: build test lint fmt vet check tools

tools:
	$(GO) get -tool mvdan.cc/gofumpt
	$(GO) get -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint

fmt:
	$(GO) tool gofumpt -l -w .

vet:
	$(GO) vet ./...

lint:
	$(GO) tool golangci-lint run

test:
	$(GO) test -race ./...

build:
	$(GO) build -o bin/taskd ./cmd/taskd

check: vet lint test
	@test -z "$$($(GO) tool gofumpt -l .)" || { echo "gofumpt found unformatted files:"; $(GO) tool gofumpt -l .; exit 1; }
