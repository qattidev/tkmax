.DEFAULT_GOAL := help

GO ?= go
ARGS ?=

.PHONY: help build install run deps fmt test vet check smoke clean

help:
	@printf '%s\n' \
	  'make install   Install tkmax into GOBIN (default: GOPATH/bin)' \
	  'make build     Build ./tkmax' \
	  'make run       Build and run tkmax; pass options with ARGS="..."' \
	  'make deps      Download Go module dependencies' \
	  'make fmt       Format Go source files' \
	  'make test      Run tests with the race detector' \
	  'make vet       Run Go static checks' \
	  'make check     Run tests and static checks' \
	  'make smoke     Test the installed Codex binary (opt-in integration test)' \
	  'make clean     Remove the local build and coverage output'

build:
	$(GO) build -o tkmax ./cmd/tkmax

install:
	$(GO) install ./cmd/tkmax

run: build
	./tkmax $(ARGS)

deps:
	$(GO) mod download

fmt:
	$(GO) fmt ./...

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: test vet

smoke:
	TKMAX_CODEX_SMOKE=1 $(GO) test ./internal/harness -run TestInstalled -count=1 -v

clean:
	$(RM) tkmax coverage.out
