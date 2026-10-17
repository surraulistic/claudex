package supervise

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
)

func TestSilenceWakeThatDidNotStartIsDeferredNotCounted(t *testing.T) {
	th := "01a0a268-ca14-7441-844b-8fcd24cd2e45"
	codexHomeFor(t, th)
	app := codextest.Start(t)
	app.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive}})
	j := newJournal(t)
	silentAssignment(t, j, "43bc6e11", th, 10*time.Minute)
	now := time.Now()
	s := New(Options{Journal: j, Now: func() time.Time { return now }})
	for i := 0; i < 5; i++ {
		if got := s.Once(context.Background()); len(got.Silent) != 0 {
			t.Fatalf("тред занят, ход не начат — это не «разбужен»: %+v", got)
		}
	}
	if n := app.Count("initialize"); n != 1 {
		t.Fatalf("одна попытка, дальше отсрочка, а не попытка каждый тик: %d", n)
	}
	if s.Status().Woken != 0 {
		t.Fatalf("счётчик разбуженных не врёт: %+v", s.Status())
	}

	app.Set(th, func(x *codextest.Thread) { x.Status = codex.ThreadStatus{Type: codex.StatusIdle} })
	now = now.Add(BackoffBase + time.Second)
	if got := s.Once(context.Background()); len(got.Silent) != 1 {
		t.Fatalf("после отсрочки тред свободен — разбужен: %+v", got)
	}
	if app.Count("thread/queue/add") != 1 {
		t.Fatalf("сообщение одно: %v", app.Calls())
	}
}

func TestSilenceAttemptsPerTickAreBounded(t *testing.T) {
	th := "01a0a268-ca14-7441-844b-8fcd24cd2e45"
	codexHomeFor(t, th)
	app := codextest.Start(t)
	app.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive}})
	j := newJournal(t)
	for i := 0; i < 8; i++ {
		silentAssignment(t, j, fmt.Sprintf("43bc6e%02d", i), th, 10*time.Minute)
	}
	s := New(Options{Journal: j, MaxPerTick: 3})
	s.Once(context.Background())
	if n := app.Count("initialize"); n != 3 {
		t.Fatalf("за тик не больше MaxPerTick попыток, а не удач: %d", n)
	}
}
