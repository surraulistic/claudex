package task

import (
	"os"
	"testing"
)

// TestMain отвязывает набор от сессии, в которой его запускают.
//
// claudex собирают изнутри Claude Code, и там выставлен CLAUDE_CODE_SESSION_ID.
// Пока выбор адреса его не читал, это было безвредно; с появлением адресации по
// разговору два давних теста начали падать — не потому, что сломались, а
// потому, что унаследовали идентификатор запускавшей сессии. Тест, исход
// которого зависит от того, кто его запустил, не доказывает ничего.
//
// Всё, что тесту нужно, он выставляет сам через t.Setenv.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN",
		"CLAUDE_CONFIG_DIR",
		"CLAUDECODE",
		"CODEX_THREAD_ID",
		"HERDR_PANE_ID",
	} {
		os.Unsetenv(k)
	}
	isolateCodex()
	os.Exit(m.Run())
}

// isolateCodex не даёт тесту дотянуться до живого Codex: подделки выставляют
// свой сокет сами.
func isolateCodex() {
	os.Setenv("CLAUDEX_CODEX_SOCKET", "/nonexistent/claudex-test/app-server.sock")
	os.Setenv("CLAUDEX_CODEX_BIN", "/nonexistent/claudex-test/codex")
}
