package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/surraulistic/claudex/internal/journal"
)

// withStdin подставляет stdin хуку на время теста.
func withStdin(t *testing.T, data []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = prev; f.Close() })
}

func hookHome(t *testing.T) *journal.Journal {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDEX_TASK_ID", "")
	return journal.Open(defaultJournal())
}

func TestHookMarksItsOwnSessionAndSkipsStrangers(t *testing.T) {
	j := hookHome(t)
	j.Append(journal.Record{Task: "465a77fc", Event: journal.Started, PaneSession: workerSession})
	withStdin(t, []byte(`{"hook_event_name":"Stop","session_id":"00000000-1111-2222-3333-444444444444","last_assistant_message":"x"}`))
	hookStop(opts{})
	withStdin(t, []byte(`{"hook_event_name":"Stop","session_id":"`+workerSession+`","last_assistant_message":"готово"}`))
	hookStop(opts{})
	recs, _ := j.Read()
	var idle []journal.Record
	for _, r := range recs {
		if r.Event == journal.Idle {
			idle = append(idle, r)
		}
	}
	if len(idle) != 1 || idle[0].PaneSession != workerSession || idle[0].Reason != "готово" {
		t.Fatalf("отметка только у сессии с поручением, получено %+v", idle)
	}
}

func TestOversizedStopPayloadIsLoggedNotSwallowed(t *testing.T) {
	j := hookHome(t)
	j.Append(journal.Record{Task: "465a77fc", Event: journal.Started, PaneSession: workerSession})
	big := `{"session_id":"` + workerSession + `","last_assistant_message":"` +
		strings.Repeat("я", hookStdinLimit/2+1) + `"}`
	withStdin(t, []byte(big))
	if err := hookStop(opts{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(defaultJournal()), "hook.log"))
	if err != nil || !strings.Contains(string(b), "больше") {
		t.Fatalf("причина видна в hook.log, получено %q %v", b, err)
	}
}
