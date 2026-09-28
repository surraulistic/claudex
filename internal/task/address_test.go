package task

import (
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

// addrRegistry кладёт живые разговоры и возвращает каталог реестра.
func addrRegistry(t *testing.T, ids ...string) string {
	t.Helper()
	home := t.TempDir()
	sock, err := os.MkdirTemp("/tmp", "ad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	for i, id := range ids {
		pid := 5000 + i
		p := filepath.Join(sock, fmt.Sprintf("%d.sock", pid))
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		rec, _ := json.Marshal(map[string]any{
			"pid": pid, "sessionId": id, "name": "license", "cwd": "/w",
			"status": "idle", "messagingSocketPath": p,
			"statusUpdatedAt": time.Now().UnixMilli(),
		})
		os.WriteFile(filepath.Join(home, fmt.Sprintf("%d.json", pid)), rec, 0o600)
	}
	return home
}

const addrSess = "7e403273-2034-4296-a833-d176bd30e03d"

func TestBareConversationIdIsRecognised(t *testing.T) {
	// Ради этого всё и затевалось: `claudex send 7e403273 "…"` должно работать
	// без флага. Раньше это отвечало «панель 7e403273 не найдена».
	home := addrRegistry(t, addrSess)
	for _, raw := range []string{addrSess, "7e403273"} {
		a, err := Classify(raw, home, nil)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if a.Kind != AddrSession || a.Session.ID != addrSess {
			t.Fatalf("%q — разговор, получено %+v", raw, a)
		}
	}
}

func TestKnownTaskIdMeansContinuation(t *testing.T) {
	home := addrRegistry(t)
	recs := []journal.Record{{Task: "cc669134", Event: journal.Started, PaneSession: addrSess}}
	a, err := Classify("cc669134", home, recs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AddrTask || a.Task != "cc669134" {
		t.Fatalf("продолжение поручения, получено %+v", a)
	}
}

func TestSameStringMatchingBothIsRefused(t *testing.T) {
	// Восемь шестнадцатеричных — и форма идентификатора поручения, и начало
	// идентификатора разговора. Одно отправит работнику, другое продолжит
	// чужую работу: угадывать нельзя.
	home := addrRegistry(t, "cc669134-0000-0000-0000-000000000000")
	recs := []journal.Record{{Task: "cc669134", Event: journal.Started, PaneSession: addrSess}}
	_, err := Classify("cc669134", home, recs)
	var amb *AmbiguousAddress
	if !errors.As(err, &amb) {
		t.Fatalf("отказ по двусмысленности, получено %v", err)
	}
	for _, want := range []string{"--session", "claudex task"} {
		if !strings.Contains(amb.Error(), want) {
			t.Errorf("в отказе сказано, как поступить (%q), получено %q", want, amb.Error())
		}
	}
}

func TestNameFallsThroughToThePane(t *testing.T) {
	// Имена разговора и панели обычно совпадают. Если бы разговор забирал и
	// имена, панель тихо лишилась бы своей привычной работы.
	home := addrRegistry(t, addrSess) // разговор назван «license»
	a, err := Classify("license", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AddrPane {
		t.Fatalf("имя — по-прежнему панель, получено %+v", a)
	}
}

func TestUnknownTaskIdIsNotTreatedAsContinuation(t *testing.T) {
	// Восемь шестнадцатеричных, которых журнал не знает, — не поручение.
	home := addrRegistry(t)
	a, err := Classify("deadbeef", home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != AddrPane {
		t.Fatalf("незнакомое уходит панели, получено %+v", a)
	}
}

func TestExecutorOfTaskIsTakenFromTheRecord(t *testing.T) {
	recs := []journal.Record{{Task: "cc669134", Event: journal.Started, PaneSession: addrSess}}
	if who, ok := ExecutorOfTask(recs, "cc669134"); !ok || who != addrSess {
		t.Fatalf("исполнитель найден, получено %q %v", who, ok)
	}
	if _, ok := ExecutorOfTask(recs, "ffffffff"); ok {
		t.Fatal("чужого не выдумываем")
	}
}
