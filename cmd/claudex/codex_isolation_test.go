package main

import (
	"os"
	"testing"
)

// TestMain не даёт тесту дотянуться до живого Codex и наблюдателя.
func TestMain(m *testing.M) {
	os.Setenv("CLAUDEX_CODEX_SOCKET", "/nonexistent/claudex-test/app-server.sock")
	os.Setenv("CLAUDEX_CODEX_BIN", "/nonexistent/claudex-test/codex")
	// Иначе отправка в тесте запустила бы тестовый бинарь наблюдателем.
	os.Setenv("CLAUDEX_NO_AUTOSUPERVISOR", "1")
	os.Exit(m.Run())
}
