package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// F12: недоставленное ведущему-панели наблюдатель не досылает, и doctor не
// обещает обратного.
func TestDoctorDoesNotPromiseToResendToAPane(t *testing.T) {
	j := isolatedSession(t, true)
	t.Setenv("HERDR_SOCKET_PATH", t.TempDir()+"/нет.sock")
	t.Setenv("CLAUDEX_CODEX_SOCKET", t.TempDir()+"/нет.sock")
	now := time.Now()
	for _, r := range []journal.Record{
		{Task: "aaaaaaaa", Event: journal.Started, Target: "wE:p17", TargetKind: task.KindPane, Time: now},
		{Task: "aaaaaaaa", Event: journal.Reported, Outcome: "готово"},
		{Task: "aaaaaaaa", Event: journal.Notified, Target: "wE:p17", TargetKind: task.KindPane,
			Stage: task.StageReported, Outcome: task.ToldHuman, Cause: "wrong_conversation"},
		{Task: "bbbbbbbb", Event: journal.Started, Target: "тред", TargetKind: task.KindThread, Time: now},
		{Task: "bbbbbbbb", Event: journal.Reported, Outcome: "готово"},
		{Task: "bbbbbbbb", Event: journal.Notified, Target: "тред", TargetKind: task.KindThread,
			Stage: task.StageReported, Outcome: task.ToldHuman, Cause: task.CauseThreadNotLive},
	} {
		j.Append(r)
	}

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	cmdDoctor(opts{pretty: true})
	w.Close()
	os.Stdout = stdout
	b, _ := io.ReadAll(r)
	var got struct {
		Checks []struct{ Name, Note string } `json:"checks"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	notes := map[string]string{}
	for _, c := range got.Checks {
		notes[c.Name] = c.Note
	}
	if n := notes["недоставленные отчёты"]; !strings.HasPrefix(n, "1 — досылает наблюдатель") {
		t.Fatalf("наблюдатель досылает только отчёт в тред, получено %q", n)
	}
	if n := notes["недоставленные ведущему-панели"]; !strings.HasPrefix(n, "1 — не досылаются") {
		t.Fatalf("панель названа отдельно и честно, получено %q", n)
	}
}
