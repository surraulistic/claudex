package main

import (
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// F5: --notify-session у панельного делегирования с ожиданием и у --detach
// не терялся молча.
func TestPanelWaitReturnsToTheNamedSession(t *testing.T) {
	j := isolatedSession(t, true)
	t.Setenv("CLAUDEX_NO_AUTOSUPERVISOR", "1")
	f := herdrtest.Start(t)
	t.Setenv("HERDR_SOCKET_PATH", f.Path)
	pane := `{"pane_id":"wE:p2","agent":"claude","agent_status":"idle","agent_session":{"value":"s-1"}}`
	f.Reply("agent.list", `{"id":"x","result":{"type":"agent_list","agents":[`+pane+`]}}`)
	f.Reply("agent.get", `{"id":"x","result":{"type":"agent","agent":`+pane+`}}`)
	f.Reply("agent.prompt", `{"id":"x","result":{"type":"agent","agent":`+pane+`}}`)

	o := opts{panel: true, notifySession: "worker", timeout: 5 * time.Second, notifyWait: time.Second}
	if err := resolveNotifySession(&o); err != nil || o.notifySession != workerSession {
		t.Fatalf("имя разговора заменено идентификатором, получено %q %v", o.notifySession, err)
	}
	go func() {
		// Исполнитель отчитывается сам, как только поручение заведено.
		for i := 0; i < 200; i++ {
			recs, _ := j.Read()
			for _, r := range recs {
				if r.Event == journal.Started {
					j.Append(journal.Record{Task: r.Task, Event: journal.Reported, Outcome: "готово", Reason: "сборка зелёная"})
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if err := cmdDelegate(o, "wE:p2", "почини сборку"); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	var started, notified bool
	for _, r := range recs {
		if r.Event == journal.Started {
			started = r.Target == workerSession && r.TargetKind == task.KindSession
		}
		if r.Event == journal.Notified && r.Target == workerSession && task.ReachedLeader(r.Outcome) {
			notified = true
		}
	}
	if !started {
		t.Fatalf("адрес возврата — названный разговор: %+v", recs)
	}
	if !notified {
		t.Fatalf("результат доставлен в названный разговор: %+v", recs)
	}
}

func TestDetachedChildKeepsTheNamedSessionAndThePanel(t *testing.T) {
	args := strings.Join(detachArgs(opts{notifySession: workerSession, timeoutRaw: "1800",
		notifyWaitRaw: "1800"}, "wE:p2", "задача"), " ")
	if !strings.Contains(args, "--notify-session "+workerSession) {
		t.Fatalf("потомку передан разговор возврата: %s", args)
	}
	// Без --panel потомок уходил в разговорный транспорт, а не ждал панель.
	if !strings.Contains(args, "--panel") {
		t.Fatalf("потомок идёт панельным путём: %s", args)
	}
}

func TestDetachToAGoneSessionIsRefusedUpFront(t *testing.T) {
	isolatedSession(t, true)
	j := journal.Open(defaultJournal())
	if err := wakeIsPossible(j, opts{detach: true, notifySession: "0000000099999999"}); err == nil {
		t.Fatal("разговора возврата нет — отказ до отправки")
	}
	if err := wakeIsPossible(j, opts{detach: true, notifySession: workerSession}); err != nil {
		t.Fatalf("живой разговор — можно, получено %v", err)
	}
}
