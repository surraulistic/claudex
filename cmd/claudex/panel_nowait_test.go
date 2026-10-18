package main

import (
	"strings"
	"testing"

	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// R5: herdr не принял agent.prompt у `delegate --panel --no-wait`. Поручение
// не должно остаться призраком в состоянии sent, а текст — пропасть.
func TestPanelNoWaitPromptRefusalLeavesNoGhostTask(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "HERDR_PANE_ID", "CODEX_THREAD_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDEX_NO_AUTOSUPERVISOR", "1")
	f := herdrtest.Start(t)
	t.Setenv("HERDR_SOCKET_PATH", f.Path)
	pane := `{"pane_id":"wE:p2","agent":"claude","agent_status":"idle","agent_session":{"value":"s-1"}}`
	f.Reply("agent.list", `{"id":"x","result":{"type":"agent_list","agents":[`+pane+`]}}`)
	f.Reply("agent.get", `{"id":"x","result":{"type":"agent","agent":`+pane+`}}`)
	f.Reply("agent.prompt", `{"id":"x","error":{"code":"agent_not_found","message":"панель ушла"}}`)

	if err := cmdDelegate(opts{panel: true, noWait: true}, "wE:p2", "почини сборку"); err == nil {
		t.Fatal("herdr отказал — это ошибка")
	}
	recs, _ := journal.Open(defaultJournal()).Read()
	if a := task.ActiveFor(recs, ""); len(a) != 0 {
		t.Fatalf("неотправленное поручение не активно: %+v", a)
	}
	var id string
	for _, r := range recs {
		if r.Event == journal.Started {
			id = r.Task
			if r.PaneSession != "s-1" {
				t.Fatalf("разговор панели записан, как у ожидающей ветки: %+v", r)
			}
		}
	}
	if st := task.StateOf(recs, id); st.State == task.StateSent {
		t.Fatalf("не отправлено — не sent: %+v", st)
	}
	var kept bool
	for _, r := range task.RefusedTasks(recs) {
		kept = kept || (r.Task == id && strings.Contains(r.Prompt, "почини сборку") &&
			strings.Contains(r.Reason, "панель ушла"))
	}
	if !kept {
		t.Fatalf("текст и причина видны в undelivered: %+v", task.RefusedTasks(recs))
	}
}
