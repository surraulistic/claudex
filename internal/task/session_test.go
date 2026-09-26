package task

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

const workerSession = "44d5f44b-e510-4236-9344-e72e5e7377f4"

// claudeHome заводит реестр сессий Claude Code с живыми сокетами и возвращает
// канал полученного каждой из них.
func claudeHome(t *testing.T, ids ...string) map[string]<-chan string {
	t.Helper()
	cfg := t.TempDir()
	home := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	// Путь юникс-сокета ограничен сотней байт, а t.TempDir() на macOS уже
	// съедает половину — сокеты живут отдельно, в коротком каталоге.
	sockDir, err := os.MkdirTemp("/tmp", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	out := map[string]<-chan string{}
	for i, id := range ids {
		pid := 1000 + i
		sock := filepath.Join(sockDir, fmt.Sprintf("%d.sock", pid))
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
		out[id] = got

		rec, _ := json.Marshal(map[string]any{
			"pid": pid, "sessionId": id, "name": "worker" + fmt.Sprint(i),
			"cwd": "/w", "kind": "bg", "status": "idle", "messagingSocketPath": sock,
			"statusUpdatedAt": time.Now().UnixMilli(),
		})
		if err := os.WriteFile(filepath.Join(home, fmt.Sprintf("%d.json", pid)), rec, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func sessionJournal(t *testing.T) *journal.Journal {
	t.Helper()
	return journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
}

func TestReportReachesTheConversationNotThePane(t *testing.T) {
	// Ради этого третий адрес и заводился: панель переживает смену агента, и
	// «панель свободна» разговора не доказывает. Сокет и есть разговор.
	inbox := claudeHome(t, workerSession)
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", workerSession,
		"Поручение 260031f8 закончено: миграции применены.",
		DeliverOptions{Stage: StageReported, Kind: KindSession, WantSession: workerSession})

	if !d.OK {
		t.Fatalf("доставлено, получено %+v", d)
	}
	select {
	case raw := <-inbox[workerSession]:
		if !strings.Contains(raw, "миграции применены") {
			t.Fatalf("текст уехал целиком, получено %q", raw)
		}
		if !strings.Contains(raw, `"type":"user"`) {
			t.Fatalf("кадр тот, что принимает Claude Code, получено %q", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("разговор получил отчёт")
	}
}

func TestVanishedConversationIsRefusedAndKept(t *testing.T) {
	// Сессия умирает между заведением поручения и отчётом. Молчаливый успех
	// здесь хуже отказа: отчёт считался бы доставленным и выпал бы из дожима.
	claudeHome(t) // реестр пуст
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession,
			WantSession: workerSession, HumanTold: true})

	if d.OK {
		t.Fatal("пропавший разговор — отказ, а не успех")
	}
	if d.Cause != CauseSessionGone {
		t.Fatalf("причина названа, получено %q (%s)", d.Cause, d.Reason)
	}
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatalf("отчёт ждёт в канале вытягивания, получено %+v", recs)
	}
}

func TestSessionReportIsDedupedLikeAnyOther(t *testing.T) {
	// Повторный done не должен будить адресата второй раз: дедупликация по
	// паре задача+стадия общая для всех трёх видов адреса.
	inbox := claudeHome(t, workerSession)
	j := sessionJournal(t)
	o := DeliverOptions{Stage: StageReported, Kind: KindSession, WantSession: workerSession}

	Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт", o)
	recs, _ := j.Read()
	if len(LostReports(recs)) != 0 {
		t.Fatal("доставленный не висит в канале вытягивания")
	}
	<-inbox[workerSession]
	select {
	case raw := <-inbox[workerSession]:
		t.Fatalf("второго сообщения не было, получено %q", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestAddressedConversationIsTheOneRecorded(t *testing.T) {
	// Рядом живёт другой разговор, и промахнуться в него нельзя: отчёт
	// уехал бы тому, кто поручения не давал.
	const neighbour = "aaaaaaaa-0000-0000-0000-000000000000"
	inbox := claudeHome(t, workerSession, neighbour)
	j := sessionJournal(t)

	Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession, WantSession: workerSession})

	select {
	case <-inbox[workerSession]:
	case <-time.After(3 * time.Second):
		t.Fatal("свой разговор получил отчёт")
	}
	select {
	case raw := <-inbox[neighbour]:
		t.Fatalf("соседу не писали, получено %q", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

// abandonedSocket оставляет файл сокета без слушателя: connect в него даёт
// отказ. Иначе этот случай не поставить — Go снимает файл вместе со
// слушателем, и «сокет есть, а принять некому» не воспроизводится.
func abandonedSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "9.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	return p
}

func TestSendFailureIsNotReportedAsDelivery(t *testing.T) {
	// Разговор жив по реестру, а сокет уже никого не слушает: сессия успела
	// кончиться. Объявить это доставкой значит потерять отчёт — он выпал бы
	// из дожима навсегда.
	cfg := t.TempDir()
	home := filepath.Join(cfg, "sessions")
	os.MkdirAll(home, 0o700)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	rec, _ := json.Marshal(map[string]any{
		"pid": 9, "sessionId": workerSession, "name": "worker", "status": "idle",
		"messagingSocketPath": abandonedSocket(t),
	})
	os.WriteFile(filepath.Join(home, "9.json"), rec, 0o600)
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession,
			WantSession: workerSession, HumanTold: true})

	if d.OK {
		t.Fatal("сокет не принял — это отказ, а не успех")
	}
	if d.Cause != CauseSendFailed {
		t.Fatalf("причина названа, получено %q (%s)", d.Cause, d.Reason)
	}
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatal("отчёт ждёт в канале вытягивания")
	}
}

func TestPrefixMatchingSeveralConversationsIsItsOwnRefusal(t *testing.T) {
	// «Разговор пропал» и «под начало идентификатора подходит несколько» —
	// разные беды: первую лечит дожим, вторую только человек.
	const a = "5d2958e0-18e2-4193-afa1-2da28def3b14"
	const b = "5d2958e0-99e2-4193-afa1-2da28def3b14"
	claudeHome(t, a, b)
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", "5d2958e0", "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession, HumanTold: true})

	if d.Cause != CauseSessionAmbiguous {
		t.Fatalf("неоднозначность названа своей причиной, получено %q (%s)", d.Cause, d.Reason)
	}
	if !strings.Contains(d.Reason, "18e2") || !strings.Contains(d.Reason, "99e2") {
		t.Fatalf("в отказе названы оба, получено %q", d.Reason)
	}
}

func TestDigestIsAppliedToWhatGoesIntoTheConversation(t *testing.T) {
	// Сводка плюс дайджест работы — то же, что уезжает в тред Codex. Слать в
	// разговор голую сводку значит отдать меньше, чем собрано.
	inbox := claudeHome(t, workerSession)
	j := sessionJournal(t)

	Deliver(context.Background(), nil, j, "260031f8", workerSession, "готово",
		DeliverOptions{Stage: StageReported, Kind: KindSession,
			Compose: func(s string) string { return s + "\n\nЧто делалось: правка конфигурации" }})

	select {
	case raw := <-inbox[workerSession]:
		if !strings.Contains(raw, "правка конфигурации") {
			t.Fatalf("дайджест уехал вместе со сводкой, получено %q", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("разговор получил отчёт")
	}
}

func TestOwnConversationOutranksThePane(t *testing.T) {
	// claudex, запущенный внутри Claude Code, должен возвращать отчёт в свой
	// разговор, а не в панель, которая его держит: за панелью со временем
	// встаёт другой разговор, за идентификатором — всегда тот же.
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	t.Setenv("HERDR_PANE_ID", "wE:p13")

	w := ResolveWake(nil, "", "", "")
	if w.Kind != KindSession || w.Target != workerSession {
		t.Fatalf("адрес — свой разговор, получено %+v", w)
	}
}

func TestNamedPaneStillWinsOverOwnConversation(t *testing.T) {
	// Названный адрес всегда сильнее угаданного: если человек сказал панель,
	// значит панель.
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	if w := ResolveWake(nil, "wE:p2", "", ""); w.Kind != KindPane || w.Target != "wE:p2" {
		t.Fatalf("названная панель выигрывает, получено %+v", w)
	}
}
