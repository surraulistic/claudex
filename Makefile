BIN := $(HOME)/.local/bin/claudex
REV := $(shell git rev-parse HEAD)
DIRTY := $(shell test -z "$$(git status --porcelain)" && echo false || echo true)
DIFF := $(shell { git diff HEAD; git ls-files --others --exclude-standard | xargs cat 2>/dev/null; } | shasum -a 256 | cut -c1-64)
LDFLAGS := -s -w -X main.buildRevision=$(REV) -X main.buildDirty=$(DIRTY) -X main.buildDiff=$(DIFF)

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o claudex ./cmd/claudex

# Как install.sh: бинарь и копия skill той же ревизии.
install:
	./install.sh

test:
	go vet ./... && go test -race ./...

.PHONY: build install test
