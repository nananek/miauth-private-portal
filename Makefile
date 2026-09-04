.PHONY: check fmt fmt-check vet test test-race build run tidy

GO_FILES := $(shell git ls-files '*.go')

check: fmt-check vet test

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@diff="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$diff" ]; then \
		echo "gofmt needed on:"; echo "$$diff"; exit 1; \
	fi

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

build:
	go build -o bin/server ./cmd/server
	go build -o bin/bootstrapctl ./cmd/bootstrapctl

run: build
	./bin/server

tidy:
	go mod tidy
