BIN := $(HOME)/.local/bin/claudex-go

build:
	go build -trimpath -ldflags '-s -w' -o claudex ./cmd/claudex

install: build
	install -m 0755 claudex $(BIN)

test:
	go vet ./... && go test -race ./...

# Переключить основную команду на эту сборку. Откат — switch-back.
switch: install
	ln -sf $(BIN) $(HOME)/.local/bin/claudex

switch-back:
	ln -sf $(HOME)/projects/claudex/bin/claudex $(HOME)/.local/bin/claudex

.PHONY: build install test switch switch-back
