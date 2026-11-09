# plugrelay developer entry points. `make check` is what CI runs.
GO      ?= go
VERSION ?= $(shell (git describe --tags --always --dirty 2>/dev/null || echo dev) | sed 's/^v//')
LDFLAGS := -s -w -X github.com/initxy/plugrelay/internal/version.Version=$(VERSION)

.PHONY: all build web dev demo test lint vet fmt e2e check clean

all: build

## build: compile the plugrelay binary (embeds the last console build)
build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/plugrelay ./cmd/plugrelay

## web: build the console into web/dist
web:
	cd web && npm ci && npm run build

## dev: run the relay with the example config
dev:
	$(GO) run ./cmd/plugrelay serve --config examples/relay.yaml

## demo: run a throwaway relay seeded with demo data and a fake upstream
demo:
	$(GO) run ./cmd/plugrelay demo $(DEMO_FLAGS)

## test: Go unit tests (race detector) and console unit tests
test:
	$(GO) test -race -timeout 30m ./...
	cd web && npm run test

## lint: gofmt, go vet, staticcheck, console typecheck + eslint
lint:
	scripts/lint.sh

## e2e: end-to-end tests against a real upstream (needs PLUGRELAY_E2E_* env)
e2e:
	$(GO) test -tags e2e -count=1 ./test/e2e/...

## check: every automated gate
check:
	scripts/check.sh

clean:
	rm -rf bin dist
