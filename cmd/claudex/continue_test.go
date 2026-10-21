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
// временных каталогах. listening=false — сокет есть, а слушать некому.
func isolatedWorker(t *testing.T, listening bool) *journal.Journal {
	t.Helper()
	j := isolatedSession(t, listening)
	if err := j.Append(journal.Record{Task: "465a77fc", Event: journal.Started,
		PaneSession: workerSession, Prompt: "исходное поручение"}); err != nil {
		t.Fatal(err)
	}
	return j
}

// isolatedSession — то же без заведённого поручения.
func isolatedSession(t *testing.T, listening bool) *journal.Journal {
	t.Helper()
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN", "HERDR_PANE_ID", "CODEX_THREAD_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	dir, err := os.MkdirTemp("/tmp", "cs")
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
	} else {
		// Сокет на месте, а слушать некому: так выглядит упавший разговор.
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		l.Close()
	}
	rec, _ := json.Marshal(map[string]any{"pid": 1, "sessionId": workerSession, "cwd": "/tmp",
		"kind": "interactive", "name": "worker", "status": "idle", "messagingSocketPath": sock,
		"statusUpdatedAt": time.Now().UnixMilli()})
	os.MkdirAll(filepath.Join(cfg, "sessions"), 0o755)
	if err := os.WriteFile(filepath.Join(cfg, "sessions", "1.json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	return journal.Open(defaultJournal())
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

// R4 сквозь send: у исполнителя открыто поручение ведущего A. Отправка от B
// заводит своё поручение с возвратом B; отправка от A продолжает исходное.
func TestSendFromAnotherLeaderIsTrackedSeparately(t *testing.T) {
	j := isolatedSession(t, true)
	t.Setenv("HERDR_SOCKET_PATH", t.TempDir()+"/нет.sock")
	t.Setenv("CLAUDEX_NO_AUTOSUPERVISOR", "1")
	j.Append(journal.Record{Task: "465a77fc", Event: journal.Started, PaneSession: workerSession,
		Target: leaderA.Target, TargetSession: leaderA.Session, TargetKind: leaderA.Kind, Prompt: "исходное"})

	t.Setenv("CLAUDE_CODE_SESSION_ID", leaderB.Target)
	if err := cmdSend(opts{}, workerSession, "работа от B"); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	if cs := task.Continuations(recs, "465a77fc"); len(cs) != 0 {
		t.Fatalf("чужое поручение не продолжено: %+v", cs)
	}
	var fresh journal.Record
	for _, r := range recs {
		if r.Event == journal.Started && r.Task != "465a77fc" {
			fresh = r
		}
	}
	if fresh.Task == "" || fresh.Target != leaderB.Target || fresh.TargetKind != task.KindSession ||
		fresh.PaneSession != workerSession || fresh.Prompt != "работа от B" {
		t.Fatalf("заведено отдельное поручение с возвратом B: %+v", fresh)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", leaderA.Target)
	if err := cmdSend(opts{}, workerSession, "ещё от A"); err != nil {
		t.Fatal(err)
	}
	recs, _ = j.Read()
	if cs := task.Continuations(recs, "465a77fc"); len(cs) != 1 || cs[0].Prompt != "ещё от A" {
		t.Fatalf("тот же ведущий продолжает своё: %+v", cs)
	}
}

// R9 сквозь cmdHeadless: запуск удался, сессия ещё не видна. Поручение
// активно и привязывается первым же Stop-хуком самой сессии.
func TestHeadlessLaunchWithoutListingStaysActiveAndBindsLater(t *testing.T) {
	j := isolatedSession(t, true)
	t.Setenv("HERDR_SOCKET_PATH", t.TempDir()+"/нет.sock")
	t.Setenv("CLAUDEX_NO_AUTOSUPERVISOR", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "claude"),
		[]byte("#!/bin/sh\nif [ \"$1\" = \"agents\" ]; then printf '[]'; fi\nexit 0\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	if err := cmdHeadless(opts{}, t.TempDir(), "проверь код"); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	var id string
	for _, r := range recs {
		if r.Event == journal.LaunchFailed {
			t.Fatalf("удавшийся запуск не записан отказом: %+v", r)
		}
		if r.Event == journal.Started && r.Headless {
			id = r.Task
		}
	}
	if st := task.StateOf(recs, id); st.State != task.StateSent {
		t.Fatalf("поручение ушло и ждёт отчёта: %+v", st)
	}

	const bg = "d00df00d-1111-4111-8111-111111111111"
	t.Setenv("CLAUDEX_TASK_ID", id)
	withStdin(t, []byte(`{"hook_event_name":"Stop","session_id":"`+bg+`","last_assistant_message":"работаю"}`))
	hookStop(opts{})
	recs, _ = j.Read()
	if a := task.ActiveFor(recs, bg); len(a) != 1 || a[0].Task != id {
		t.Fatalf("Stop-хук сессии привязал поручение: %+v", a)
	}
}

func TestDoneWithEnglishOutcomeIsRecordedAsFailure(t *testing.T) {
	// F3: «claudex done <id> failed: tests red» — провал, а не готово.
	j := isolatedWorker(t, true)
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	t.Setenv("CLAUDEX_NO_AUTOSUPERVISOR", "1")
	if err := cmdDone(opts{}, []string{"465a77fc", "failed:", "tests", "red"}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	st := task.StateOf(recs, "465a77fc")
	if st.State != task.StateFailed || st.Reason != "tests red" {
		t.Fatalf("провал с причиной «tests red», получено %+v", st)
	}
}
