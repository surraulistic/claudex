package main

import (
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// D1: сокет разговора не принял поручение. Поручение не должно числиться за
// этим разговором, а Stop-хук человека в нём — становиться «итогом».
func TestFailedSessionSendLeavesNothingActiveAndKeepsThePrompt(t *testing.T) {
	j := isolatedSession(t, false)
	t.Setenv("HERDR_SOCKET_PATH", t.TempDir()+"/нет.sock")
	s, err := claudesess.Lookup(claudesess.Home(), workerSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := delegateToConversation(opts{noWait: true}, s, "", "проверь миграцию 42"); err == nil {
		t.Fatal("сокет не принял — ошибка")
	}
	recs, _ := j.Read()
	if a := task.ActiveFor(recs, workerSession); len(a) != 0 {
		t.Fatalf("неотправленное поручение за разговором не числится: %+v", a)
	}
	var id string
	for _, r := range recs {
		if r.Event == journal.Started {
			id = r.Task
		}
	}
	if st := task.StateOf(recs, id); st.State == task.StateSent {
		t.Fatalf("не отправлено — не sent: %+v", st)
	}
	var kept bool
	for _, r := range task.RefusedTasks(recs) {
		kept = kept || (r.Task == id && strings.Contains(r.Prompt, "миграцию 42") && r.Reason != "")
	}
	if !kept {
		t.Fatalf("текст и причина видны в undelivered: %+v", task.RefusedTasks(recs))
	}

	withStdin(t, []byte(`{"hook_event_name":"Stop","session_id":"`+workerSession+
		`","last_assistant_message":"личная переписка человека"}`))
	hookStop(opts{})
	recs, _ = j.Read()
	for _, r := range recs {
		if r.Event == journal.Idle {
			t.Fatalf("реплика человека не стала отметкой поручения: %+v", r)
		}
	}
	if sl := task.SilentTasks(recs, time.Now().Add(time.Hour), 0); len(sl) != 0 {
		t.Fatalf("будить затеявшего не за что: %+v", sl)
	}
}
