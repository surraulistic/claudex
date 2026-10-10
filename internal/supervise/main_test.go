package supervise

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// TestMain не даёт тесту дотянуться до живого Codex: подделки выставляют свой
// сокет сами.
func TestMain(m *testing.M) {
	os.Setenv("CLAUDEX_CODEX_SOCKET", "/nonexistent/claudex-test/app-server.sock")
	os.Setenv("CLAUDEX_CODEX_BIN", "/nonexistent/claudex-test/codex")
	os.Exit(m.Run())
}

func TestOnlyOneSupervisorHoldsTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "supervisor.lock")
	if Held(path) {
		t.Fatal("свободный замок не занят")
	}
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrRunning) {
		t.Fatalf("второй наблюдатель получает отказ, получено %v", err)
	}
	if !Held(path) {
		t.Fatal("занятый замок виден")
	}
	release()
	if Held(path) {
		t.Fatal("после выхода замок свободен")
	}
}

func TestEveryTickFollowsStartedTurns(t *testing.T) {
	var seen []task.FlushOptions
	n := 0
	s := New(Options{Journal: newJournal(t), Flush: spyFlush(true, &seen),
		Follow: func(context.Context, *journal.Journal, int) []task.FollowedTurn {
			n++
			return []task.FollowedTurn{{Task: "43bc6e11", Status: "completed"}}
		}})
	got := s.Once(context.Background())
	if n != 1 || len(got.TurnsDone) != 1 || s.Status().TurnsDone != 1 {
		t.Fatalf("завершение ходов проверяется каждый тик: вызовов %d, %+v", n, got)
	}
}
