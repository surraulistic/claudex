package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/supervise"
	"github.com/surraulistic/claudex/internal/task"
)

const pendingThread = "01a0a51e-20c4-7e10-8488-b524b9389489"

// pendingThreadReport — отчёт, ждущий дожима в тред, и app-server, который
// принимает соединения и молчит. Всё во временных каталогах.
func pendingThreadReport(t *testing.T) *int32 {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "нет.sock"))
	codexHome := t.TempDir()
	os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(`{"id":"`+pendingThread+`"}`+"\n"), 0o644)
	t.Setenv("CODEX_HOME", codexHome)
	dir, _ := os.MkdirTemp("/tmp", "cx")
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var dials int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&dials, 1)
			t.Cleanup(func() { c.Close() })
		}
	}()
	t.Setenv("CLAUDEX_CODEX_SOCKET", filepath.Join(dir, "s.sock"))
	j := journal.Open(defaultJournal())
	for _, r := range []journal.Record{
		{Task: "465a77fc", Event: journal.Started, Time: time.Now().Add(-time.Hour),
			Target: pendingThread, TargetSession: pendingThread, TargetKind: task.KindThread},
		{Task: "465a77fc", Event: journal.Finished, Outcome: task.SentNoWait},
		{Task: "465a77fc", Event: journal.Reported, Outcome: "готово"},
		{Task: "465a77fc", Event: journal.Notified, Target: pendingThread, TargetKind: task.KindThread,
			Stage: task.StageReported, Outcome: task.NotDelivered, Cause: task.CauseQueueFailed},
	} {
		j.Append(r)
	}
	return &dials
}

func TestInlineUpkeepLeavesFlushToARunningSupervisor(t *testing.T) {
	dials := pendingThreadReport(t)
	release, err := supervise.Acquire(supervise.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	inlineUpkeep(opts{})
	if n := atomic.LoadInt32(dials); n != 0 {
		t.Fatalf("при работающем наблюдателе чужой отчёт не дожимается в команде: %d", n)
	}
}

func TestInlineUpkeepWithoutSupervisorIsBounded(t *testing.T) {
	dials := pendingThreadReport(t)
	start := time.Now()
	inlineUpkeep(opts{})
	if el := time.Since(start); el > inlineBudget+2*time.Second {
		t.Fatalf("попутный дожим ограничен, заняло %s", el)
	}
	if atomic.LoadInt32(dials) == 0 {
		t.Fatal("без наблюдателя попутный дожим всё-таки пробует")
	}
}

// D11: файл состояния называет живой чужой процесс, а замок свободен —
// наблюдателя нет, и попутный дожим всё-таки идёт.
func TestInlineUpkeepDoesNotTrustAStalePidFile(t *testing.T) {
	dials := pendingThreadReport(t)
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	b, _ := json.Marshal(supervise.Status{PID: other.Process.Pid})
	os.MkdirAll(filepath.Dir(supervise.StatusPath()), 0o755)
	os.WriteFile(supervise.StatusPath(), b, 0o644)
	inlineUpkeep(opts{})
	if atomic.LoadInt32(dials) == 0 {
		t.Fatal("pid из файла без замка — не наблюдатель")
	}
}

// D8: сортировка вывода не переставляет общий кэш журнала.
func TestTasksOutputDoesNotReorderTheCachedJournal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	j := journal.Open(defaultJournal())
	now := time.Now()
	j.Append(journal.Record{Task: "bbbb0002", Event: journal.Started, Time: now})
	j.Append(journal.Record{Task: "aaaa0001", Event: journal.Started, Time: now.Add(-time.Hour)})
	before, _ := j.Read()
	order := before[0].Task + before[1].Task
	cmdTasks(opts{all: true}, nil)
	after, _ := j.Read()
	if after[0].Task+after[1].Task != order {
		t.Fatalf("порядок записей в кэше прежний: %s → %s", order, after[0].Task+after[1].Task)
	}
}
