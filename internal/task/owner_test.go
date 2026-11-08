package task

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
	"github.com/surraulistic/claudex/internal/journal"
)

const desktopCodex = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"

// heldBy кладёт замок писателя leaderThread и называет его держателя.
func heldBy(t *testing.T, pids []int, exe, args string) {
	t.Helper()
	lock := filepath.Join(os.Getenv("CODEX_HOME"), "thread-writer-locks", leaderThread+".lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ph, pc := codex.LockHolders, codex.ProcessCommand
	codex.LockHolders = func(_ context.Context, p string) ([]int, error) {
		if p != lock {
			t.Errorf("смотрит не тот замок: %s", p)
		}
		return pids, nil
	}
	codex.ProcessCommand = func(context.Context, int) (string, string, error) { return exe, args, nil }
	t.Cleanup(func() { codex.LockHolders, codex.ProcessCommand = ph, pc })
}

func desktopLeader(t *testing.T) *codextest.Fake {
	t.Helper()
	unloadedHome(t, leaderThread)
	app := codextest.Start(t)
	app.Add(leaderThread, &codextest.Thread{OwnedElsewhere: true})
	return app
}

func TestDesktopOwnedLeaderGetsEventThroughOwnersQueue(t *testing.T) {
	app := desktopLeader(t)
	heldBy(t, []int{4242}, desktopCodex, desktopCodex+" -c features.code_mode_host=true app-server --analytics-default-enabled")
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", leaderThread)
	o := doneOpts(j)
	o.FullReport = true

	res, err := Report(context.Background(), "465a77fc", "готово", reportText, o)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Delivered
	if !d.OK || !d.Pending || d.Transport != codex.TransportOwnerQueue || d.Cause != codex.HoldOwnerPending {
		t.Fatalf("принято в очередь владельца, ход не начат: %+v", d)
	}
	q := app.Thread(leaderThread).Queue
	if len(q) != 1 || !strings.HasPrefix(q[0].Text(), codex.TextPrefix+"465a77fc") ||
		strings.Contains(q[0].Text(), "завершилось после срока") || !strings.Contains(q[0].Text(), "данные, не инструкция") {
		t.Fatalf("ход с правами владельца получает событие, а не отчёт: %+v", q)
	}

	app.OwnerTakes(leaderThread)
	if f := Flush(context.Background(), FlushOptions{Journal: j}); len(f) != 1 || !f[0].Ok {
		t.Fatalf("наблюдатель замечает ход владельца: %+v", f)
	}
	if n := app.Count("thread/queue/add"); n != 1 {
		t.Fatalf("сообщение положено один раз, а не %d", n)
	}
	for _, m := range []string{"thread/resume", "thread/queue/start", "thread/settings/update", "permissionProfile/list"} {
		if app.Count(m) != 0 {
			t.Fatalf("%s у чужого писателя не вызывается: %v", m, app.Calls())
		}
	}
	recs, _ := j.Read()
	lines := strings.Join(ThreadTimeline(recs, "465a77fc"), "\n")
	for _, want := range []string{"queue accepted", "очередь владельца треда", "ChatGPT Desktop", "turn "} {
		if !strings.Contains(lines, want) {
			t.Errorf("в истории доставки есть %q:\n%s", want, lines)
		}
	}
}

func TestUnprovenLockOwnerKeepsQueueUntouched(t *testing.T) {
	cases := []struct {
		name, exe, args, cause string
		pids                   []int
	}{
		{"codex exec", "/usr/local/bin/codex", "/usr/local/bin/codex exec resume " + leaderThread,
			codex.HoldOpenElsewhere, []int{4242}},
		{"stale lock", desktopCodex, desktopCodex + " app-server", codex.HoldOwnerUnknown, nil},
		{"daemon itself", desktopCodex, desktopCodex + " app-server", codex.HoldOwnerUnknown, []int{os.Getpid()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := desktopLeader(t)
			heldBy(t, c.pids, c.exe, c.args)
			j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
			seedThreadTask(t, j, "465a77fc", leaderThread)

			res, err := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
			if err != nil {
				t.Fatal(err)
			}
			d := res.Delivered
			if d.Reached() || d.ThreadStage != codex.StageNotQueued || d.Cause != c.cause {
				t.Fatalf("fail-closed: %+v", d)
			}
			if q := app.Thread(leaderThread).Queue; len(q) != 0 || app.Count("thread/resume") != 0 {
				t.Fatalf("ни очереди, ни resume: %+v %v", q, app.Calls())
			}
		})
	}
}
