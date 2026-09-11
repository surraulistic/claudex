BIN := $(HOME)/.local/bin/claudex

build:
	go build -trimpath -ldflags '-s -w' -o claudex ./cmd/claudex

install: build
	install -m 0755 claudex $(BIN)

test:
	go vet ./... && go test -race ./...

.PHONY: build install test
