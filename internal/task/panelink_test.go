package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

// paneRegistry кладёт в реестр один живой разговор и возвращает его id.
func paneRegistry(t *testing.T, ids ...string) {
	t.Helper()
	cfg := t.TempDir()
	home := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sock, err := os.MkdirTemp("/tmp", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	for i, id := range ids {
		pid := 3000 + i
		p := filepath.Join(sock, fmt.Sprintf("%d.sock", pid))
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		rec, _ := json.Marshal(map[string]any{
			"pid": pid, "sessionId": id, "name": "worker", "cwd": "/w",
			"kind": "interactive", "status": "idle", "messagingSocketPath": p,
			"statusUpdatedAt": time.Now().UnixMilli(),
		})
		os.WriteFile(filepath.Join(home, fmt.Sprintf("%d.json", pid)), rec, 0o600)
	}
}

const paneConversation = "7e403273-2034-4296-a833-d176bd30e03d"

func TestPaneResolvesToTheConversationItRunsNow(t *testing.T) {
	// Главное правило новой адресации: панель называет адресата, а поручение
	// уезжает разговору, который она ведёт сейчас.
	paneRegistry(t, paneConversation)
	got, err := ConversationOfPane("wE:p13", "claude", paneConversation)
	if err != nil {
		t.Fatalf("панель разрешилась в разговор, получено %v", err)
	}
	if got.ID != paneConversation {
		t.Fatalf("тот самый разговор, получено %+v", got)
	}
}

func TestAmbiguousPaneConversationIsRefusedNotGuessed(t *testing.T) {
	// Промах уводит поручение в чужой разговор, поэтому выбор наугад запрещён.
	paneRegistry(t,
		"5d2958e0-18e2-4193-afa1-2da28def3b14",
		"5d2958e0-99e2-4193-afa1-2da28def3b14")
	_, err := ConversationOfPane("wE:p13", "claude", "5d2958e0")
	var r *PaneRefusal
	if !errors.As(err, &r) || r.Cause != RefuseAmbiguous {
		t.Fatalf("отказ по неоднозначности, получено %v", err)
	}
	if !strings.Contains(r.Reason, "18e2") || !strings.Contains(r.Reason, "99e2") {
		t.Fatalf("названы оба кандидата, получено %q", r.Reason)
	}
}

func TestPaneWithoutAProvenConversationIsRefused(t *testing.T) {
	// herdr не сообщил разговор: доказать, кому адресовано поручение, нечем.
	paneRegistry(t, paneConversation)
	_, err := ConversationOfPane("wE:p13", "claude", "")
	var r *PaneRefusal
	if !errors.As(err, &r) || r.Cause != RefuseIdentityUnproven {
		t.Fatalf("отказ по недоказанной привязке, получено %v", err)
	}
}

func TestNonClaudePaneIsRefusedWithItsOwnCause(t *testing.T) {
	// В панели может идти не Claude: разговора у неё нет, и подменять его
	// нечем. Прежний транспорт остаётся доступен явным --panel.
	paneRegistry(t, paneConversation)
	_, err := ConversationOfPane("wE:p13", "codex", paneConversation)
	var r *PaneRefusal
	if !errors.As(err, &r) || r.Cause != RefuseNotClaude {
		t.Fatalf("отказ «не Claude», получено %v", err)
	}
	if !strings.Contains(r.Reason, "--panel") {
		t.Fatalf("сказано, чем пользоваться взамен, получено %q", r.Reason)
	}
}

func TestClosedConversationOfALivePaneIsRefused(t *testing.T) {
	// Панель жива, а разговор в ней закрыт: писать некому.
	paneRegistry(t, paneConversation)
	_, err := ConversationOfPane("wE:p13", "claude", "aaaaaaaa-0000-0000-0000-000000000000")
	var r *PaneRefusal
	if !errors.As(err, &r) || r.Cause != RefuseNoConversation {
		t.Fatalf("отказ «разговора нет», получено %v", err)
	}
}

func TestOwnPaneIsNotADelegationTarget(t *testing.T) {
	// Поручать самому себе нечего, и доводить до отказа Claude Code незачем.
	t.Setenv("CLAUDE_CODE_SESSION_ID", paneConversation)
	paneRegistry(t, paneConversation)
	_, err := ConversationOfPane("wE:p13", "claude", paneConversation)
	var r *PaneRefusal
	if !errors.As(err, &r) || r.Cause != RefuseSelf {
		t.Fatalf("отказ «это текущий разговор», получено %v", err)
	}
}

func TestConversationDelegationNeedsNoHerdrAtAll(t *testing.T) {
	// Прежний транспорт требовал герольда на каждом шаге. Разговор адресуется
	// без него — а значит поручение не может походя завести панель: заводить
	// её нечем.
	paneRegistry(t, paneConversation)
	s, err := ConversationOfPane("wE:p13", "claude", paneConversation)
	if err != nil {
		t.Fatal(err)
	}
	if s.Socket == "" {
		t.Fatal("адрес доставки — сокет разговора")
	}
	// Доставка тем же путём, что и отчёт: клиент herdr не передаётся вовсе.
	j := sessionJournal(t)
	j.Append(journal.Record{Task: "cafebabe", Event: journal.Started,
		PaneSession: s.ID, Target: s.ID, TargetSession: s.ID, TargetKind: KindSession,
		Time: time.Now()})
	if err := SentWithoutWaiting(j, "cafebabe", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	d := Deliver(context.Background(), nil, j, "cafebabe", s.ID, "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession, HumanTold: true})
	if !d.OK {
		t.Fatalf("доставка прошла без herdr, получено %+v", d)
	}
}

func TestOldJournalRecordsStillCorrelateByPane(t *testing.T) {
	// Совместимость с прежними вызовами: записи, заведённые до адресации по
	// разговору, не знают поля разговора вовсе и обязаны сопоставляться как
	// раньше — по панели.
	recs := []journal.Record{
		{Task: "0a972b3a", Event: journal.Started, Pane: "wE:p1A", Time: time.Now()},
	}
	if a := ActiveFor(recs, "wE:p1A"); len(a) != 1 || a[0].Task != "0a972b3a" {
		t.Fatalf("старая запись находится по панели, получено %+v", a)
	}
	r, err := Resolve(recs, "wE:p1A", "0a972b3a")
	if err != nil || r.Task != "0a972b3a" {
		t.Fatalf("и отчитывается по ней же, получено %+v / %v", r, err)
	}
	if w := WakeOf("", "wE:p1A", "разговор"); w.Kind != KindPane {
		t.Fatalf("пустой вид по-прежнему читается как панель, получено %+v", w)
	}
}
