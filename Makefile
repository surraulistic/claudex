BIN := $(HOME)/.local/bin/claudex
REV := $(shell git rev-parse HEAD)
DIRTY := $(shell test -z "$$(git status --porcelain)" && echo false || echo true)
DIFF := $(shell { git diff HEAD; git ls-files --others --exclude-standard | xargs cat 2>/dev/null; } | shasum -a 256 | cut -c1-64)
LDFLAGS := -s -w -X main.buildRevision=$(REV) -X main.buildDirty=$(DIRTY) -X main.buildDiff=$(DIFF)

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o claudex ./cmd/claudex

install: build
	install -m 0755 claudex $(BIN)

test:
	go vet ./... && go test -race ./...

.PHONY: build install test
