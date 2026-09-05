.PHONY: check fmt fmt-check vet test test-race build run tidy contract-test

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
	go build -o bin/miauthctl ./cmd/miauthctl
	go build -o bin/jobsctl ./cmd/jobsctl
	go build -o bin/mailfetch ./cmd/mailfetch
	go build -o bin/backupctl ./cmd/backupctl

run: build
	./bin/server

tidy:
	go mod tidy

# contract-test runs Issue #7's misskey_dart contract test suite against
# a real bin/server (scripts/run-contract-tests.sh; see fetch_doc
# key='plan' section 9 and docs/compat/aria-v1.5.11.md's "Issue #7
# implementation notes"). It approves the test session through miauthctl.
# Requires the Dart SDK in addition to this target's own Go build.
contract-test:
	./scripts/run-contract-tests.sh
