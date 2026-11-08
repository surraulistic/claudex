package codex_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
)

const desktopExe = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"

func desktopOwner() codex.Owner {
	return codex.Owner{Kind: codex.OwnerAppServer, PID: 4242, Exe: desktopExe,
		Args: desktopExe + " -c features.code_mode_host=true app-server --analytics-default-enabled"}
}

func ownedBy(o codex.Owner) codex.WakeRequest {
	q := req()
	q.Owner = func() codex.Owner { return o }
	return q
}

// Всё, чем claudex стал бы вторым писателем или поменял бы права чужой сессии.
var writerCalls = []string{"permissionProfile/list", "thread/resume", "thread/settings/update", "thread/queue/start"}

func desktopThread(th codextest.Thread) *codextest.Thread {
	th.OwnedElsewhere = true
	return &th
}

func TestDesktopOwnedThreadIsOnlyQueuedForItsOwner(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{}))

	r, err := codex.Wake(context.Background(), ownedBy(desktopOwner()))
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != codex.StageAccepted || r.Hold != codex.HoldOwnerPending ||
		r.Transport != codex.TransportOwnerQueue || r.Started() {
		t.Fatalf("принято, но не начато — так и сказано: %+v", r)
	}
	if !strings.Contains(r.Reason, "ChatGPT Desktop") || !strings.Contains(r.Reason, "со своими правами") {
		t.Errorf("причина называет владельца и чьи права: %q", r.Reason)
	}
	q := f.Thread(th).Queue
	if len(q) != 1 || q[0].ClientID != req().ClientID {
		t.Fatalf("в общей очереди ровно наше сообщение: %+v", q)
	}
	untouched(t, f, writerCalls...)
}

func TestDesktopOwnerStartsTurnFromSharedQueue(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{OwnerTakesAfter: 20 * time.Millisecond, KeepRunning: true}))
	q := ownedBy(desktopOwner())
	q.StartWait = 2 * time.Second

	r, err := codex.Wake(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != codex.StageStarted || r.TurnID == "" || r.Transport != codex.TransportOwnerQueue {
		t.Fatalf("ход владельца виден в общей истории: %+v", r)
	}
	if f.Thread(th).Status.Type != codex.StatusNotLoaded {
		t.Fatalf("демон тред не загружал: %+v", f.Thread(th).Status)
	}
	untouched(t, f, writerCalls...)
}

func TestOwnersRunningTurnIsNotMistakenForInterrupted(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{KeepRunning: true}))
	codex.Wake(context.Background(), ownedBy(desktopOwner()))
	f.OwnerTakes(th)
	f.SeenFromOutside(th)

	q := ownedBy(desktopOwner())
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if r.Stage != codex.StageStarted || r.Hold != "" {
		t.Fatalf("ход идёт у владельца — начат, а не оборван: %+v", r)
	}
	f.Set(th, func(x *codextest.Thread) {
		x.Turns[0].Status, x.Turns[0].CompletedAt = codex.TurnInterrupted, time.Now().Unix()
	})
	if r, _ := codex.Wake(context.Background(), q); r.Stage != codex.StageTurnFailed {
		t.Fatalf("настоящий обрыв с completedAt — turn_failed: %+v", r)
	}
}

func TestOwnerRouteRetryFindsOwnersTurnWithoutRequeue(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{}))
	first, _ := codex.Wake(context.Background(), ownedBy(desktopOwner()))
	if first.Started() {
		t.Fatalf("владелец ещё не брал: %+v", first)
	}
	f.OwnerTakes(th)

	q := ownedBy(desktopOwner())
	q.Accepted = true
	r, err := codex.Wake(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != codex.StageCompleted || r.TurnID == "" {
		t.Fatalf("повтор узнаёт ход владельца: %+v", r)
	}
	if n := f.Count("thread/queue/add"); n != 1 {
		t.Fatalf("сообщение положено один раз, а не %d: %v", n, f.Calls())
	}
	untouched(t, f, writerCalls...)
}

func TestOwnerTookMessageButHistoryLagsStaysPending(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{}))
	q := ownedBy(desktopOwner())
	q.Accepted = true

	r, _ := codex.Wake(context.Background(), q)
	if r.Stage != codex.StageAccepted || r.Hold != codex.HoldOwnerPending {
		t.Fatalf("забранное владельцем без видимого хода — ждём, а не теряем: %+v", r)
	}
	untouched(t, f, append([]string{"thread/queue/add"}, writerCalls...)...)
}

func TestOwnerQueueGetsEventNotExecutorReport(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{}))
	q := ownedBy(desktopOwner())
	q.Text = codex.TextPrefix + "aaaa1111 — полный отчёт исполнителя"
	q.QueueOnlyText = codex.TextPrefix + "aaaa1111 — отчёт готов. Прочитай сам: claudex task aaaa1111"

	codex.Wake(context.Background(), q)
	if got := f.Thread(th).Queue; len(got) != 1 || got[0].Text() != q.QueueOnlyText {
		t.Fatalf("ход с чужими правами получает только событие: %+v", got)
	}
}

func TestOwnerRouteDoesNotRefuseOverHumanQueue(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{Queue: []codex.QueuedItem{{ID: "u1", ClientID: "app-123",
		Input: []codex.UserInput{{Type: "text", Text: "сообщение человека"}}}}}))

	r, _ := codex.Wake(context.Background(), ownedBy(desktopOwner()))
	if r.Hold != codex.HoldOwnerPending {
		t.Fatalf("очередь вычитывает владелец, не claudex: %+v", r)
	}
	if q := f.Thread(th).Queue; len(q) != 2 || q[0].ID != "u1" {
		t.Fatalf("своё — в хвост, чужое не тронуто: %+v", q)
	}
}

func TestUnprovenOwnerIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		owner codex.Owner
		hold  string
	}{
		{"codex exec", codex.Owner{Kind: codex.OwnerForeign, PID: 4242, Exe: "/usr/local/bin/codex",
			Why: "держатель замка — codex, но не app-server"}, codex.HoldOpenElsewhere},
		{"unknown", codex.Owner{Kind: codex.OwnerUnknown, Why: "lsof: нет"}, codex.HoldOwnerUnknown},
		{"stale lock", codex.Owner{Kind: codex.OwnerUnknown,
			Why: "замок писателя есть, а держащего его процесса не видно"}, codex.HoldOwnerUnknown},
		{"daemon itself", codex.Owner{Kind: codex.OwnerAppServer, PID: os.Getpid(), Exe: "/x/codex"},
			codex.HoldOwnerUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := codextest.Start(t)
			f.Add(th, desktopThread(codextest.Thread{}))
			r, err := codex.Wake(context.Background(), ownedBy(c.owner))
			if err != nil {
				t.Fatal(err)
			}
			if r.Hold != c.hold || r.Stage != codex.StageNotQueued {
				t.Fatalf("fail-closed: %+v", r)
			}
			untouched(t, f, append([]string{"thread/queue/add"}, writerCalls...)...)
		})
	}
}

func TestOwnerQueueCanBeSwitchedOff(t *testing.T) {
	t.Setenv("CLAUDEX_CODEX_OWNER_QUEUE", "off")
	f := codextest.Start(t)
	f.Add(th, desktopThread(codextest.Thread{}))
	r, _ := codex.Wake(context.Background(), ownedBy(desktopOwner()))
	if r.Hold != codex.HoldOpenElsewhere || r.Stage != codex.StageNotQueued {
		t.Fatalf("выключено — прежний отказ: %+v", r)
	}
	untouched(t, f, append([]string{"thread/queue/add"}, writerCalls...)...)
}

// Режим remote-control: тред загружен в самом демоне — владелец он, и замок
// не спрашивается.
func TestDaemonLoadedThreadNeverAsksOwner(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, watched(codextest.Thread{Profile: codex.WakePermissionProfile, ResumeSandbox: "workspaceWrite"}))
	asked := false
	q := req()
	q.Owner = func() codex.Owner { asked = true; return desktopOwner() }

	r, err := codex.Wake(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if asked || !r.Started() || r.Transport != codex.TransportAppServer {
		t.Fatalf("загруженный в демоне тред будится через демон: %+v asked=%v", r, asked)
	}
}

func TestPeerPIDIsTheServerProcess(t *testing.T) {
	f := codextest.Start(t)
	s, err := codex.DialSocket(context.Background(), f.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.PeerPID(); got != os.Getpid() {
		t.Fatalf("pid за сокетом %d, а сервер — этот процесс %d", got, os.Getpid())
	}
}
