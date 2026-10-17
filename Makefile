SHELL := /bin/sh
GO ?= go
VERSION ?= dev
LDFLAGS := -s -w -X github.com/agyard/agsudo/internal/cli.Version=$(VERSION)

.PHONY: build demo test check integration install clean
build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/agsudo ./cmd/agsudo
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o bin/agsudo-helper ./cmd/agsudo-helper

demo: build
	CGO_ENABLED=0 $(GO) build -tags dev -trimpath -o bin/agsudo-helper-dev ./cmd/agsudo-helper
	@echo 'Development mode only: no privilege elevation.'
	@echo 'export AGSUDO_HELPER="$(CURDIR)/bin/agsudo-helper-dev" AGSUDO_DEV_STATE="$(CURDIR)/.agsudo-dev"'

test:
	$(GO) test -race -count=1 ./...

check:
	@unformatted=$$(gofmt -l cmd internal); test -z "$$unformatted" || { echo "$$unformatted"; exit 1; }
	$(GO) mod tidy -diff
	$(GO) vet ./...

integration: demo
	python3 tests/integration.py
	python3 tests/privileged.py

install:
	./scripts/install.sh $(ALLOW_UID)

clean:
	rm -rf bin
