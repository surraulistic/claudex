package codex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
)

func TestMain(m *testing.M) {
	os.Setenv("CLAUDEX_CODEX_SOCKET", "/nonexistent/claudex-test/app-server.sock")
	os.Setenv("CLAUDEX_CODEX_BIN", "/nonexistent/claudex-test/codex")
	os.Exit(m.Run())
}

const th = "01a0cccc-0000-7000-8000-000000000001"

func req() codex.WakeRequest {
	return codex.WakeRequest{Thread: th, ClientID: "claudex-aaaa1111-reported", Task: "aaaa1111",
		Text: codex.TextPrefix + "aaaa1111 — готово", StartWait: 50 * time.Millisecond,
		Poll: 5 * time.Millisecond, Each: 2 * time.Second}
}

func stubQueue(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := codex.Queue
	codex.Queue = func(context.Context, string, string) error { n++; return nil }
	t.Cleanup(func() { codex.Queue = prev })
	return &n
}

func TestUnloadedThreadIsResumedAndDrainsOurMessage(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{})

	r, err := codex.Wake(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if r.Stage != codex.StageCompleted || r.TurnID == "" || r.Transport != codex.TransportAppServer {
		t.Fatalf("ход начат и виден, получено %+v", r)
	}
	if f.Count("thread/resume") != 1 || f.Count("thread/queue/start") != 0 {
		t.Fatalf("resume вычитывает очередь сам, queue/start не нужен: %v", f.Calls())
	}
}

func TestRetryNeverQueuesTwice(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{})
	codex.Wake(context.Background(), req())
	r, err := codex.Wake(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if f.Count("thread/queue/add") != 1 {
		t.Fatalf("повтор не кладёт второе сообщение: %v", f.Calls())
	}
	if !r.Started() {
		t.Fatalf("повтор узнаёт уже начатый ход, получено %+v", r)
	}
}

func mineQueued() codex.QueuedItem {
	return codex.QueuedItem{ID: "own1", ClientID: req().ClientID,
		Input: []codex.UserInput{{Type: "text", Text: req().Text}}}
}

func before(t *testing.T, calls []string, first, then string) {
	t.Helper()
	i, j := -1, -1
	for k, c := range calls {
		if c == first && i < 0 {
			i = k
		}
		if c == then && j < 0 {
			j = k
		}
	}
	if i < 0 || j < 0 || i > j {
		t.Fatalf("%s должен идти раньше %s: %v", first, then, calls)
	}
}

func untouched(t *testing.T, f *codextest.Fake, methods ...string) {
	t.Helper()
	for _, m := range methods {
		if f.Count(m) != 0 {
			t.Fatalf("%s не вызывается: %v", m, f.Calls())
		}
	}
}

var touching = []string{"thread/queue/add", "permissionProfile/list", "thread/resume",
	"thread/settings/update", "thread/queue/start"}

func idle(th codextest.Thread) *codextest.Thread {
	th.Status = codex.ThreadStatus{Type: codex.StatusIdle}
	return &th
}

// watched — загруженный idle тред, открытый в приложении на другом профиле.
func watched(th codextest.Thread) *codextest.Thread {
	th.Watched = true
	if th.Profile == "" {
		th.Profile = ":workspace"
	}
	return idle(th)
}

func TestLoadedIdleThreadIsSwitchedToClaudexWorkBeforeQueueAdd(t *testing.T) {
	cases := []struct {
		name string
		th   codextest.Thread
	}{
		{"notice after ack", codextest.Thread{}},
		{"notice before ack", codextest.Thread{SettingsNoticeFirst: true}},
		{"notice delayed within Each", codextest.Thread{SettingsNoticeDelay: 150 * time.Millisecond}},
		{"full access before", codextest.Thread{Profile: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := codextest.Start(t)
			c.th.Cwd = "/work/repo"
			tt := idle(c.th)
			tt.Watched = true
			f.Add(th, tt)
			r, err := codex.Wake(context.Background(), req())
			if err != nil {
				t.Fatal(err)
			}
			if r.Stage != codex.StageCompleted || r.Hold != "" || r.TurnID == "" {
				t.Fatalf("тред взял сообщение сам, ход виден: %+v", r)
			}
			calls := f.Calls()
			before(t, calls, "permissionProfile/list", "thread/resume")
			before(t, calls, "thread/resume", "thread/settings/update")
			before(t, calls, "thread/settings/update", "thread/queue/add")
			untouched(t, f, "thread/queue/start")
			want := map[string]any{"threadId": th, "permissions": codex.WakePermissionProfile}
			if got := f.SettingsUpdates(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("update ровно с профилем claudex-work и без sandbox: %v", got)
			}
			wantResume := map[string]any{"threadId": th, "excludeTurns": true, "permissions": codex.WakePermissionProfile}
			if got := f.Resumes(); len(got) != 1 || !reflect.DeepEqual(got[0], wantResume) {
				t.Fatalf("resume ровно с профилем claudex-work: %v", got)
			}
			if len(f.ProfileCwds) != 1 || f.ProfileCwds[0] != "/work/repo" {
				t.Fatalf("профиль проверяется в cwd треда: %v", f.ProfileCwds)
			}
			if got := f.Thread(th).Profile; got != codex.WakePermissionProfile {
				t.Fatalf("ход идёт под claudex-work, профиль %q", got)
			}
		})
	}
}

// Задачи 3a560c46 и 6a1d858b: тред открыт в приложении и уже на claudex-work
// после прошлого пробуждения. update ничего не меняет, уведомления нет ни
// для кого — профиль подтверждает ответ resume.
func TestLoadedThreadAlreadyOnClaudexWorkIsWokenWithoutSettingsUpdate(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, watched(codextest.Thread{Profile: codex.WakePermissionProfile, ResumeSandbox: "workspaceWrite"}))
	r, err := codex.Wake(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Started() || r.Hold != "" || f.Count("thread/queue/add") != 1 {
		t.Fatalf("профиль назван resume, ход начат: %+v %v", r, f.Calls())
	}
	before(t, f.Calls(), "thread/resume", "thread/queue/add")
	untouched(t, f, "thread/settings/update", "thread/queue/start")
}

func TestUnwatchedLoadedIdleThreadIsReloadedWithClaudexWork(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, idle(codextest.Thread{}))
	r, err := codex.Wake(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Started() || r.Hold != "" || f.Thread(th).Profile != codex.WakePermissionProfile {
		t.Fatalf("ничей idle тред перезагружен resume с claudex-work: %+v %v", r, f.Calls())
	}
	before(t, f.Calls(), "thread/resume", "thread/queue/add")
	untouched(t, f, "thread/settings/update", "thread/queue/start")
}

func TestLoadedIdleThreadWithoutConfirmedProfileGetsNothingQueued(t *testing.T) {
	cases := []struct {
		name     string
		th       codextest.Thread
		profiles []codex.PermissionProfile
		hold     string
	}{
		{"profile disallowed", codextest.Thread{}, []codex.PermissionProfile{{ID: "claudex-work"}}, codex.HoldNoProfile},
		{"resume error", codextest.Thread{FailResume: true}, nil, codex.HoldLoadedProfile},
		{"update error", codextest.Thread{FailSettings: true}, nil, codex.HoldLoadedProfile},
		{"no notification", codextest.Thread{NoSettingsNotice: true}, nil, codex.HoldLoadedProfile},
		{"notification for another thread", codextest.Thread{SettingsNoticeFor: "01a0cccc-0000-7000-8000-000000000002"},
			nil, codex.HoldLoadedProfile},
		{"notification later than Each", codextest.Thread{SettingsNoticeDelay: time.Second}, nil, codex.HoldLoadedProfile},
		{"update without effect", codextest.Thread{IgnorePermissions: true}, nil, codex.HoldLoadedProfile},
		{"wrong profile", codextest.Thread{SettingsProfile: ":read-only"}, nil, codex.HoldWrongProfile},
		{"full access, no profile", codextest.Thread{SettingsProfile: codextest.NoProfile}, nil, codex.HoldWrongProfile},
		{"right profile, full access", codextest.Thread{SettingsSandbox: "dangerFullAccess"}, nil, codex.HoldWrongProfile},
		{"right profile, external sandbox", codextest.Thread{SettingsSandbox: "externalSandbox"}, nil, codex.HoldWrongProfile},
		{"already claudex-work, full access", codextest.Thread{Profile: codex.WakePermissionProfile,
			ResumeSandbox: "dangerFullAccess"}, nil, codex.HoldWrongProfile},
		{"already claudex-work, external sandbox", codextest.Thread{Profile: codex.WakePermissionProfile,
			ResumeSandbox: "externalSandbox"}, nil, codex.HoldWrongProfile},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := codextest.Start(t)
			if c.profiles != nil {
				f.Profiles = c.profiles
			}
			f.Add(th, watched(c.th))
			q := req()
			q.Each = 300 * time.Millisecond
			r, err := codex.Wake(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if r.Stage != codex.StageNotQueued || r.Hold != c.hold || r.Started() || r.Reason == "" {
				t.Fatalf("не положено (%s) и сказано почему, получено %+v", c.hold, r)
			}
			untouched(t, f, "thread/queue/add", "thread/queue/start")
			if got := f.Thread(th); len(got.Queue) != 0 || len(got.Turns) != 0 || got.Status.Type != codex.StatusIdle {
				t.Fatalf("ни очереди, ни хода: %+v", got)
			}
		})
	}
}

// Протокольный факт 0.162.1: thread/settings/updated приходит только
// подписчикам треда, а подписывает resume.
func TestSettingsNoticeReachesOnlySubscribedConnection(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, watched(codextest.Thread{}))
	s, err := codex.DialSocket(context.Background(), f.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err = s.SetPermissions(c, th, codex.WakePermissionProfile)
	cancel()
	if err == nil {
		t.Fatal("без подписки уведомления нет")
	}
	f.Set(th, func(tt *codextest.Thread) { tt.Profile = ":workspace" })
	rs, err := s.Resume(context.Background(), th, codex.WakePermissionProfile)
	if err != nil || rs.Profile != ":workspace" {
		t.Fatalf("открытый тред переопределения resume не принимает, профиль назван: %+v %v", rs, err)
	}
	st, err := s.SetPermissions(context.Background(), th, codex.WakePermissionProfile)
	if err != nil || st.Profile != codex.WakePermissionProfile {
		t.Fatalf("после resume уведомление приходит: %+v %v", st, err)
	}
}

func TestThreadTakenDuringSettingsUpdateGetsNothingQueued(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, watched(codextest.Thread{BusyAfterSettings: true}))
	r, _ := codex.Wake(context.Background(), req())
	if r.Stage != codex.StageNotQueued || r.Hold != codex.HoldBusy || f.Count("thread/settings/update") != 1 {
		t.Fatalf("тред занят к концу update — не кладём: %+v %v", r, f.Calls())
	}
	untouched(t, f, "thread/queue/add", "thread/queue/start")
}

func TestOwnQueuedItemOnLoadedIdleThreadIsNotStartedExplicitly(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, idle(codextest.Thread{Held: true, Queue: []codex.QueuedItem{mineQueued()}}))
	q := req()
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if r.Stage != codex.StageAccepted || r.Hold != codex.HoldNotStarted || r.QueueID != "own1" {
		t.Fatalf("своё в очереди, тред его не взял: %+v", r)
	}
	if f.Count("thread/settings/update") != 1 {
		t.Fatalf("профиль сменён и подтверждён: %v", f.Calls())
	}
	untouched(t, f, "thread/queue/add", "thread/queue/start")
	if len(f.Thread(th).Queue) != 1 {
		t.Fatalf("очередь не тронута: %+v", f.Thread(th).Queue)
	}
}

func TestOwnQueuedItemOnLoadedIdleThreadWaitsForConfirmedProfile(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, idle(codextest.Thread{Held: true, FailSettings: true, Queue: []codex.QueuedItem{mineQueued()}}))
	q := req()
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if r.Stage != codex.StageAccepted || r.Hold != codex.HoldLoadedProfile || r.QueueID != "own1" {
		t.Fatalf("своё уже в очереди, профиль не подтверждён: %+v", r)
	}
	untouched(t, f, "thread/queue/add", "thread/queue/start")
	if got := f.Thread(th); len(got.Queue) != 1 || len(got.Turns) != 0 {
		t.Fatalf("очередь не тронута, хода нет: %+v", got)
	}
}

func TestThreadsThatMustNotBeTouchedGetNothingQueued(t *testing.T) {
	foreignItem := codex.QueuedItem{ID: "u1", ClientID: "app-123",
		Input: []codex.UserInput{{Type: "text", Text: "сообщение человека"}}}
	cases := []struct {
		name string
		th   codextest.Thread
		hold string
	}{
		{"busy", codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive}}, codex.HoldBusy},
		{"approval", codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive,
			ActiveFlags: []string{"waitingOnApproval"}}}, codex.HoldWaitingHuman},
		{"user input", codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive,
			ActiveFlags: []string{"waitingOnUserInput"}}}, codex.HoldWaitingHuman},
		{"system error", codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusSystemError}}, codex.HoldSystemError},
		{"unknown", codextest.Thread{Status: codex.ThreadStatus{Type: "paused"}}, codex.HoldNotStarted},
		{"foreign queue, unloaded", codextest.Thread{Queue: []codex.QueuedItem{foreignItem}}, codex.HoldForeignQueue},
		{"foreign queue, idle", codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle}, Held: true,
			Queue: []codex.QueuedItem{foreignItem}}, codex.HoldForeignQueue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := codextest.Start(t)
			th0 := c.th
			want := len(th0.Queue)
			f.Add(th, &th0)
			r, err := codex.Wake(context.Background(), req())
			if err != nil {
				t.Fatal(err)
			}
			if r.Stage != codex.StageNotQueued || r.Hold != c.hold || r.QueueID != "" {
				t.Fatalf("не положено (%s), получено %+v", c.hold, r)
			}
			untouched(t, f, touching...)
			if got := len(f.Thread(th).Queue); got != want {
				t.Fatalf("очередь не тронута: было %d, стало %d", want, got)
			}
		})
	}
}

func TestOwnQueuedItemOnBusyThreadIsNotDuplicated(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive},
		Queue: []codex.QueuedItem{mineQueued()}})
	r, _ := codex.Wake(context.Background(), req())
	if r.Stage != codex.StageAccepted || r.Hold != codex.HoldBusy || r.QueueID != "own1" {
		t.Fatalf("своё уже в очереди, ждём: %+v", r)
	}
	if f.Count("thread/queue/add")+f.Count("thread/resume") != 0 {
		t.Fatalf("ни дубля, ни resume: %v", f.Calls())
	}
}

func TestThreadTakenDuringResumeGetsNothingQueued(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{BusyAfterResume: true})
	r, _ := codex.Wake(context.Background(), req())
	if r.Stage != codex.StageNotQueued || r.Hold != codex.HoldBusy || f.Count("thread/resume") != 1 {
		t.Fatalf("тред занят к концу resume — не кладём: %+v %v", r, f.Calls())
	}
	untouched(t, f, "thread/queue/add", "thread/queue/start")
}

func TestThreadHeldByAnotherProcessIsNotResumed(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{})
	q := req()
	q.OpenElsewhere = func() bool { return true }
	r, _ := codex.Wake(context.Background(), q)
	if r.Hold != codex.HoldOpenElsewhere || r.Stage != codex.StageNotQueued {
		t.Fatalf("второй писатель недопустим: %+v %v", r, f.Calls())
	}
	untouched(t, f, touching...)
}

func TestForeignQueueStopsColdWakeBeforeOpenElsewhereAndProfile(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Queue: []codex.QueuedItem{{ID: "u1", ClientID: "app-123",
		Input: []codex.UserInput{{Type: "text", Text: "сообщение человека"}}}}})
	asked := false
	q := req()
	q.OpenElsewhere = func() bool { asked = true; return false }
	r, _ := codex.Wake(context.Background(), q)
	if r.Hold != codex.HoldForeignQueue || r.Stage != codex.StageNotQueued || asked {
		t.Fatalf("чужая очередь — отказ сразу: %+v asked=%v", r, asked)
	}
	untouched(t, f, touching...)
}

func TestResumeWithoutDrainStartsOnlyOurItem(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{NoDrainOnResume: true, Queue: []codex.QueuedItem{mineQueued()}})
	q := req()
	q.Accepted = true
	r, err := codex.Wake(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Started() || f.Count("thread/queue/start") != 1 || f.Count("thread/queue/add") != 0 {
		t.Fatalf("своё сообщение запущено явно: %+v %v", r, f.Calls())
	}
}

func TestOurItemBehindAnotherIsNotJumpedAhead(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{NoDrainOnResume: true, Queue: []codex.QueuedItem{{ID: "c0",
		ClientID: "claudex-bbbb2222-reported", Input: []codex.UserInput{{Type: "text", Text: "x"}}},
		mineQueued()}})
	q := req()
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if r.Started() || r.Hold != codex.HoldNotStarted || f.Count("thread/queue/start") != 0 {
		t.Fatalf("очередь вне порядка не запускаем: %+v %v", r, f.Calls())
	}
}

func TestAcceptedEarlierIsNeverAddedAgain(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive}})
	q := req()
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if f.Count("thread/queue/add") != 0 || r.Hold != codex.HoldItemGone {
		t.Fatalf("исчезнувшее сообщение не кладём снова: %+v %v", r, f.Calls())
	}
}

func TestItemQueuedByCLIIsRecognisedByText(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Queue: []codex.QueuedItem{{ID: "cli1", ClientID: "random-uuid",
		Input: []codex.UserInput{{Type: "text", Text: codex.TextPrefix + "aaaa1111 — готово"}}}}})
	q := req()
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if f.Count("thread/queue/add") != 0 || r.QueueID != "cli1" {
		t.Fatalf("запись от codex queue узнаётся и не дублируется: %+v %v", r, f.Calls())
	}
	if f.Count("thread/resume") != 1 {
		t.Fatalf("своя запись без clientId чужой не считается: %v", f.Calls())
	}
}

func TestAppServerDownFallsBackToQueueOnlyAndSaysSo(t *testing.T) {
	n := stubQueue(t)
	t.Setenv("CLAUDEX_CODEX_SOCKET", filepath.Join(t.TempDir(), "нет.sock"))
	r, err := codex.Wake(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if *n != 1 || r.Transport != codex.TransportQueueOnly || r.Stage != codex.StageAccepted ||
		r.Hold != codex.HoldAppServerDown {
		t.Fatalf("очередь без хода, сказано прямо: %+v (queue вызван %d раз)", r, *n)
	}
	q := req()
	q.Accepted = true
	codex.Wake(context.Background(), q)
	if *n != 1 {
		t.Fatal("повтор при лежащем app-server не кладёт второй раз")
	}
}

func TestQueueOnlyFailureIsAnError(t *testing.T) {
	prev := codex.Queue
	codex.Queue = func(context.Context, string, string) error { return errors.New("нет") }
	t.Cleanup(func() { codex.Queue = prev })
	t.Setenv("CLAUDEX_CODEX_SOCKET", filepath.Join(t.TempDir(), "нет.sock"))
	if _, err := codex.Wake(context.Background(), req()); err == nil {
		t.Fatal("не принято нигде — это ошибка")
	}
}

func TestFollowSeesCompletion(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{KeepRunning: true})
	r, _ := codex.Wake(context.Background(), req())
	if r.Stage != codex.StageStarted || r.TurnStatus != codex.TurnInProgress {
		t.Fatalf("начат, не завершён: %+v", r)
	}
	r, _ = codex.Follow(context.Background(), th, req().ClientID, time.Second)
	if r.Stage != codex.StageStarted {
		t.Fatalf("пока идёт — started, получено %+v", r)
	}
	f.Finish(th, codex.TurnFailed)
	r, _ = codex.Follow(context.Background(), th, req().ClientID, time.Second)
	if r.Stage != codex.StageCompleted || r.TurnStatus != codex.TurnFailed {
		t.Fatalf("завершение видно со статусом, получено %+v", r)
	}
}

func TestBadThreadsAreErrors(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Ephemeral: true})
	if _, err := codex.Wake(context.Background(), req()); err == nil {
		t.Fatal("эфемерный тред — отказ")
	}
	q := req()
	q.Thread = "01a0dddd-0000-7000-8000-000000000009"
	if _, err := codex.Wake(context.Background(), q); err == nil {
		t.Fatal("неизвестный тред — отказ")
	}
	if f.Count("thread/queue/add") != 0 {
		t.Fatal("ничего не положено")
	}
}

func TestResumeFailureQueuesNothing(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{FailResume: true})
	r, err := codex.Wake(context.Background(), req())
	if err != nil || r.Stage != codex.StageNotQueued || r.Hold != codex.HoldNotStarted {
		t.Fatalf("не положено, ход не начат: %+v %v", r, err)
	}
	if f.Count("thread/queue/add") != 0 || len(f.Thread(th).Queue) != 0 {
		t.Fatalf("без подтверждённого профиля в очереди ничего: %v", f.Calls())
	}
}

func TestColdResumeSwitchesThreadToExactlyClaudexWork(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Cwd: "/work/repo"})
	r, err := codex.Wake(context.Background(), req())
	if err != nil || !r.Started() {
		t.Fatalf("ход начат: %+v %v", r, err)
	}
	want := map[string]any{"threadId": th, "excludeTurns": true, "permissions": "claudex-work"}
	if got := f.Resumes(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("resume ровно с профилем claudex-work и без sandbox: %v", got)
	}
	if got := f.Thread(th).Profile; got != "claudex-work" {
		t.Fatalf("профиль липкий и остаётся на треде, получено %q", got)
	}
	if len(f.ProfileCwds) != 1 || f.ProfileCwds[0] != "/work/repo" {
		t.Fatalf("профиль проверяется в cwd треда: %v", f.ProfileCwds)
	}
	calls := f.Calls()
	before(t, calls, "permissionProfile/list", "thread/resume")
	before(t, calls, "thread/resume", "thread/queue/add")
}

func TestColdResumeConfirmsProfileBeforeQueueAddAndStart(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Held: true})
	r, err := codex.Wake(context.Background(), req())
	if err != nil || !r.Started() {
		t.Fatalf("своё сообщение запущено явно: %+v %v", r, err)
	}
	calls := f.Calls()
	before(t, calls, "permissionProfile/list", "thread/resume")
	before(t, calls, "thread/resume", "thread/queue/add")
	before(t, calls, "thread/queue/add", "thread/queue/start")
	if f.Count("thread/queue/add") != 1 || f.Count("thread/queue/start") != 1 {
		t.Fatalf("ровно одно сообщение, ровно один start: %v", calls)
	}
	if got := f.Thread(th).Profile; got != codex.WakePermissionProfile {
		t.Fatalf("ход идёт под claudex-work, профиль %q", got)
	}
}

func TestUnusableProfileFailsClosedBeforeResume(t *testing.T) {
	cases := []struct {
		name     string
		profiles []codex.PermissionProfile
		fail     bool
	}{
		{"missing", []codex.PermissionProfile{{ID: ":workspace", Allowed: true}}, false},
		{"disallowed", []codex.PermissionProfile{{ID: "claudex-work", Allowed: false}}, false},
		{"list error", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := codextest.Start(t)
			f.Profiles, f.FailProfiles = c.profiles, c.fail
			f.Add(th, &codextest.Thread{})
			r, err := codex.Wake(context.Background(), req())
			if err != nil {
				t.Fatal(err)
			}
			if r.Stage != codex.StageNotQueued || r.Hold != codex.HoldNoProfile {
				t.Fatalf("не положено, не запущено: %+v", r)
			}
			if f.Count("permissionProfile/list") != 1 {
				t.Fatalf("профиль спрошен: %v", f.Calls())
			}
			untouched(t, f, "thread/queue/add", "thread/resume", "thread/queue/start")
			if th := f.Thread(th); len(th.Queue) != 0 || th.Profile != "" {
				t.Fatalf("очередь и профиль треда не тронуты: %+v", th)
			}
		})
	}
}

func TestUnusableProfileLeavesOwnQueuedItemUnstarted(t *testing.T) {
	f := codextest.Start(t)
	f.FailProfiles = true
	f.Add(th, &codextest.Thread{Queue: []codex.QueuedItem{mineQueued()}})
	q := req()
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if r.Stage != codex.StageAccepted || r.Hold != codex.HoldNoProfile || r.QueueID != "own1" {
		t.Fatalf("своё в очереди, но не будим: %+v", r)
	}
	if f.Count("thread/queue/add")+f.Count("thread/resume")+f.Count("thread/queue/start") != 0 ||
		len(f.Thread(th).Queue) != 1 {
		t.Fatalf("ни дубля, ни resume, ни start: %v", f.Calls())
	}
}

func TestResumeThatIgnoresProfileFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		th   codextest.Thread
	}{
		{"profile not named, unloaded", codextest.Thread{IgnorePermissions: true, NoDrainOnResume: true}},
		{"profile not named, queue held", codextest.Thread{IgnorePermissions: true, Held: true}},
		{"full access sandbox", codextest.Thread{NoDrainOnResume: true, ResumeSandbox: "dangerFullAccess"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := codextest.Start(t)
			th0 := c.th
			f.Add(th, &th0)
			r, err := codex.Wake(context.Background(), req())
			if err != nil {
				t.Fatal(err)
			}
			if r.Started() || r.Stage != codex.StageNotQueued || r.Hold != codex.HoldWrongProfile {
				t.Fatalf("сервер не подтвердил профиль — дальше не идём: %+v", r)
			}
			if f.Count("thread/queue/add")+f.Count("thread/queue/start") != 0 || len(f.Thread(th).Queue) != 0 {
				t.Fatalf("ни queue/add, ни queue/start: %v", f.Calls())
			}
		})
	}
}

func TestWrongProfileDoesNotStartOwnQueuedItem(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{IgnorePermissions: true, NoDrainOnResume: true,
		Queue: []codex.QueuedItem{mineQueued()}})
	q := req()
	q.Accepted = true
	r, _ := codex.Wake(context.Background(), q)
	if r.Started() || r.Hold != codex.HoldWrongProfile || r.Stage != codex.StageAccepted {
		t.Fatalf("профиль не подтверждён: %+v", r)
	}
	if f.Count("thread/queue/add")+f.Count("thread/queue/start") != 0 || len(f.Thread(th).Queue) != 1 {
		t.Fatalf("своё не дублируем и не стартуем: %v", f.Calls())
	}
}

func TestClientRefusesResumeWithoutProfile(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{})
	s, err := codex.DialSocket(context.Background(), f.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Resume(context.Background(), th, ""); err == nil {
		t.Fatal("resume без профиля — ошибка")
	}
	if f.Count("thread/resume") != 0 {
		t.Fatalf("на провод не ушло: %v", f.Calls())
	}
}

func TestClientRefusesSettingsUpdateWithoutProfile(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, idle(codextest.Thread{}))
	s, err := codex.DialSocket(context.Background(), f.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.SetPermissions(context.Background(), th, ""); err == nil {
		t.Fatal("update без профиля — ошибка")
	}
	if f.Count("thread/settings/update") != 0 {
		t.Fatalf("на провод не ушло: %v", f.Calls())
	}
}
