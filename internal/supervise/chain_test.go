package supervise

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// Сквозная проверка всей цепочки.
//
// Отправили работу → исполнитель доработал ход и НЕ вызвал done → хук записал
// отметку → наблюдатель пережил срок молчания и разбудил затеявшего событием →
// затеявший может прочитать подробности.
//
// Каждое звено проверено по отдельности, но целиком цепочка до сих пор
// проверялась только руками. Правка в середине развалила бы её молча — этот
// тест и написан против такой правки.

// worker поднимает живой разговор в реестре: и исполнителя, и затеявшего.
func worker(t *testing.T, id string) <-chan string {
	t.Helper()
	cfg := t.TempDir()
	home := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	dir, err := os.MkdirTemp("/tmp", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "1.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	got := make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b := make([]byte, 32<<10)
			n, _ := c.Read(b)
			got <- string(b[:n])
			c.Close()
		}
	}()
	rec, _ := json.Marshal(map[string]any{
		"pid": 1, "sessionId": id, "name": "worker", "cwd": "/w",
		"status": "idle", "messagingSocketPath": sock,
		"statusUpdatedAt": time.Now().UnixMilli(),
	})
	if err := os.WriteFile(filepath.Join(home, "1.json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	return got
}

const chainSession = "7e403273-2034-4296-a833-d176bd30e03d"

func TestWholeChainFromSendToWakeWithoutAnyDone(t *testing.T) {
	inbox := worker(t, chainSession)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	now := time.Now()

	// 1. Работа отправлена: исполнитель и затеявший — один и тот же разговор,
	//    чтобы проверка не зависела от чужих сессий.
	j.Append(journal.Record{
		Task: "43bc6e11", Event: journal.Started, PaneSession: chainSession,
		Target: chainSession, TargetSession: chainSession, TargetKind: task.KindSession,
		Prompt: "задача", Time: now.Add(-time.Hour),
	})
	if err := task.SentWithoutWaiting(j, "43bc6e11", ""); err != nil {
		t.Fatal(err)
	}
	if got := task.StateOf(mustRead(t, j), "43bc6e11").State; got != task.StateSent {
		t.Fatalf("после отправки — sent, получено %s", got)
	}

	// 2. Исполнитель доработал ход и промолчал: это записал бы хук Stop.
	j.Append(journal.Record{
		Task: "43bc6e11", Event: journal.Idle, PaneSession: chainSession,
		Reason: "ветка запушена, миграции применены", Time: now.Add(-10 * time.Minute),
	})

	// 3. Наблюдатель пережидает молчание и будит затеявшего.
	s := New(Options{Journal: j, StatusPath: filepath.Join(t.TempDir(), "s.json")})
	tick := s.Once(context.Background())
	if len(tick.Silent) != 1 || tick.Silent[0] != "43bc6e11" {
		t.Fatalf("наблюдатель разбудил за молчуна, получено %+v", tick)
	}

	// 4. В разговор пришло событие: что случилось и где читать — но без оценки.
	select {
	case raw := <-inbox:
		if !strings.Contains(raw, "43bc6e11") {
			t.Fatalf("в событии назван идентификатор, получено %q", raw)
		}
		if !strings.Contains(raw, "ветка запушена") {
			t.Fatalf("последняя реплика передана доказательством, получено %q", raw)
		}
		if strings.Contains(raw, "готово") || strings.Contains(raw, "провал") {
			t.Fatalf("исход не называем — его знает только задача, получено %q", raw)
		}
		if !strings.Contains(raw, "claudex task log 43bc6e11") {
			t.Fatalf("сказано, где читать подробности, получено %q", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("событие дошло до затеявшего")
	}

	// 5. Второй проход не будит второй раз.
	if again := s.Once(context.Background()); len(again.Silent) != 0 {
		t.Fatalf("повторно за то же не будим, получено %+v", again)
	}
	select {
	case raw := <-inbox:
		t.Fatalf("второго сообщения не было, получено %q", raw)
	case <-time.After(300 * time.Millisecond):
	}

	// 6. Отчёта от самой задачи так и не было — и состояние этого не скрывает.
	if got := task.StateOf(mustRead(t, j), "43bc6e11").State; got == task.StateDone {
		t.Fatal("без отчёта задачи «готово» не объявляем")
	}
}

func TestChainStillPrefersTheTaskOwnReport(t *testing.T) {
	// Наблюдение — запасной путь. Если задача отчиталась сама, её оценка
	// ценнее, и будить за молчуна не надо.
	worker(t, chainSession)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	now := time.Now()
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Started,
		PaneSession: chainSession, Target: chainSession, TargetKind: task.KindSession,
		Time: now.Add(-time.Hour)})
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Idle,
		Reason: "последняя реплика", Time: now.Add(-10 * time.Minute)})
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Reported,
		Outcome: "готово", Reason: "сделал вот это", Time: now.Add(-9 * time.Minute)})

	s := New(Options{Journal: j})
	if got := s.Once(context.Background()); len(got.Silent) != 0 {
		t.Fatalf("отчитавшийся молчуном не считается, получено %+v", got)
	}
}

func mustRead(t *testing.T, j *journal.Journal) []journal.Record {
	t.Helper()
	recs, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}
