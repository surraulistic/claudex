package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

const workerSession = "81fad209-733a-4b55-b5f2-6a8b9cc14eed"

// isolatedWorker — HOME, реестр Claude и сокет исполнителя только во
// временных каталогах. listening=false — запись реестра есть, а слушать некому.
func isolatedWorker(t *testing.T, listening bool) *journal.Journal {
	t.Helper()
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN", "HERDR_PANE_ID", "CODEX_THREAD_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "1.sock")
	if listening {
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 64<<10)
				c.Read(buf)
				c.Close()
			}
		}()
	} else if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec, _ := json.Marshal(map[string]any{"pid": 1, "sessionId": workerSession, "cwd": "/tmp",
		"kind": "interactive", "name": "worker", "status": "idle", "messagingSocketPath": sock,
		"statusUpdatedAt": time.Now().UnixMilli()})
	os.MkdirAll(filepath.Join(cfg, "sessions"), 0o755)
	if err := os.WriteFile(filepath.Join(cfg, "sessions", "1.json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	j := journal.Open(defaultJournal())
	if err := j.Append(journal.Record{Task: "465a77fc", Event: journal.Started,
		PaneSession: workerSession, Prompt: "исходное поручение"}); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestFailedContinuationKeepsItsText(t *testing.T) {
	j := isolatedWorker(t, false)
	recs, _ := j.Read()
	if err := sendContinuation(opts{}, "465a77fc", recs, "продолжи: проверь миграцию"); err == nil {
		t.Fatal("отправка в мёртвый сокет — ошибка")
	}
	recs, _ = j.Read()
	cs := task.Continuations(recs, "465a77fc")
	if len(cs) != 1 || cs[0].Prompt != "продолжи: проверь миграцию" || cs[0].Outcome != task.ContinuationFailed ||
		cs[0].Reason == "" {
		t.Fatalf("текст и отказ записаны, получено %+v", cs)
	}
	var found bool
	for _, r := range task.RefusedTasks(recs) {
		found = found || (r.Task == "465a77fc" && strings.Contains(r.Prompt, "проверь миграцию"))
	}
	if !found {
		t.Fatalf("неотправленное продолжение видно в undelivered: %+v", task.RefusedTasks(recs))
	}
	if st := task.StateOf(recs, "465a77fc"); st.Reason != "" && strings.Contains(st.Reason, "продолжение") {
		t.Fatalf("отказ продолжения не подменяет состояние поручения: %+v", st)
	}
}

func TestSentContinuationIsJournaled(t *testing.T) {
	j := isolatedWorker(t, true)
	recs, _ := j.Read()
	if err := sendContinuation(opts{}, "465a77fc", recs, "ещё: обнови README"); err != nil {
		t.Fatal(err)
	}
	recs, _ = j.Read()
	cs := task.Continuations(recs, "465a77fc")
	if len(cs) != 1 || cs[0].Outcome != task.ContinuationSent || cs[0].Prompt != "ещё: обнови README" {
		t.Fatalf("продолжение записано с исходом, получено %+v", cs)
	}
	if got := task.RefusedTasks(recs); len(got) != 0 {
		t.Fatalf("отправленное в undelivered не попадает: %+v", got)
	}
}
