package task

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
	"github.com/surraulistic/claudex/internal/journal"
)

func doneOpts(j *journal.Journal) ReportOptions {
	return ReportOptions{Journal: j, Pane: "wE:p37", Deadline: time.Second, Poll: time.Millisecond}
}

func TestQueueOnlyIsCalledQueuedNotDelivered(t *testing.T) {
	sent := codexHome(t, leaderThread)
	t.Setenv("CLAUDEX_CODEX_SOCKET", filepath.Join(t.TempDir(), "нет.sock"))
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", leaderThread)

	res, err := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || res.Delivered == nil || !res.Delivered.Pending ||
		res.Delivered.Transport != codex.TransportQueueOnly {
		t.Fatalf("codex queue принял, ход не начат, сказано прямо: %+v %+v", res.Delivered, *sent)
	}
	recs, _ := j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateQueued {
		t.Fatalf("состояние — в очереди, не доставлено и не undelivered: %+v", st)
	}
	if l := LostReports(recs); len(l) != 1 {
		t.Fatalf("пробуждение ещё дожимается, получено %+v", l)
	}
	if f := Flush(context.Background(), FlushOptions{Journal: j}); len(f) != 1 || f[0].Ok {
		t.Fatalf("принятое не выдаётся за дошедшее: %+v", f)
	}
	if len(*sent) != 1 {
		t.Fatalf("повтор при лежащем app-server не кладёт второй раз, получено %d", len(*sent))
	}
}

func TestQueuedEarlierIsWokenWithoutDuplicateWhenAppServerReturns(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	app.Add(otherThread, &codextest.Thread{Queue: []codex.QueuedItem{{ID: "cli1", ClientID: "uuid",
		Input: []codex.UserInput{{Type: "text", Text: codex.TextPrefix + "465a77fc — готово"}}}}})
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)
	j.Append(journal.Record{Task: "465a77fc", Event: journal.Reported, Outcome: "готово"})
	j.Append(journal.Record{Task: "465a77fc", Event: journal.Notified, Target: otherThread,
		TargetKind: KindThread, Stage: StageReported, Outcome: ThreadQueued,
		ThreadStage: codex.StageAccepted, Transport: codex.TransportQueueOnly, Cause: codex.HoldAppServerDown})

	f := Flush(context.Background(), FlushOptions{Journal: j})
	if len(f) != 1 || !f[0].Ok {
		t.Fatalf("app-server вернулся — ход начат: %+v", f)
	}
	if app.Count("thread/queue/add") != 0 || app.Count("thread/resume") != 1 {
		t.Fatalf("положенное codex queue не дублируется: %v", app.Calls())
	}
	recs, _ := j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateDone {
		t.Fatalf("дошло, получено %+v", st)
	}
	if l := LostReports(recs); len(l) != 0 {
		t.Fatalf("дожимать больше нечего, получено %+v", l)
	}
}

func TestBusyThreadKeepsTheMessageAndWakesLater(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	app.Add(otherThread, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusActive,
		ActiveFlags: []string{"waitingOnApproval"}}})
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)

	res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if !res.Delivered.Pending || res.Delivered.Cause != codex.HoldWaitingHuman {
		t.Fatalf("тред ждёт человека — ход не запускаем: %+v", res.Delivered)
	}
	if app.Count("thread/resume")+app.Count("thread/queue/start")+app.Count("thread/queue/add") != 0 {
		t.Fatalf("ход не трогали и в очередь не клали: %v", app.Calls())
	}

	app.Set(otherThread, func(th *codextest.Thread) { th.Status = codex.ThreadStatus{Type: codex.StatusNotLoaded} })
	Flush(context.Background(), FlushOptions{Journal: j})
	if app.Count("thread/queue/add") != 1 {
		t.Fatalf("сообщение одно: %v", app.Calls())
	}
	recs, _ := j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateDone {
		t.Fatalf("после resume дошло, получено %+v", st)
	}
	var stages []string
	for _, r := range recs {
		if r.ThreadStage != "" {
			stages = append(stages, r.ThreadStage)
		}
	}
	want := []string{codex.StageNotQueued, codex.StageStarted, codex.StageCompleted}
	if strings.Join(stages, ",") != strings.Join(want, ",") {
		t.Fatalf("стадии записаны по одной, получено %v", stages)
	}
}

// openThread — загруженный idle тред, открытый в приложении.
func openThread(profile string) *codextest.Thread {
	return &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle}, Watched: true, Profile: profile}
}

func TestLoadedIdleThreadIsWokenAfterProfileConfirmed(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	app.Add(otherThread, openThread(":workspace"))
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)

	res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if res.Delivered == nil || !res.Delivered.OK || res.Delivered.Pending ||
		res.Delivered.ThreadStage != codex.StageCompleted {
		t.Fatalf("загруженный idle тред взял событие под claudex-work: %+v", res.Delivered)
	}
	calls := strings.Join(app.Calls(), ",")
	r, i, k := strings.Index(calls, "thread/resume"), strings.Index(calls, "thread/settings/update"),
		strings.Index(calls, "thread/queue/add")
	if r < 0 || i < r || k < i {
		t.Fatalf("сначала подписка, потом профиль, потом очередь: %s", calls)
	}
	if n := app.Count("thread/queue/start"); n != 0 {
		t.Fatalf("открытый тред не стартуется явно: %s", calls)
	}
	if p := app.Thread(otherThread).Profile; p != codex.WakePermissionProfile {
		t.Fatalf("тред на claudex-work, получено %q", p)
	}
	recs, _ := j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateDone {
		t.Fatalf("дошло, получено %+v", st)
	}
}

// Как у 3a560c46/6a1d858b: тред открыт и уже на claudex-work, уведомления
// о смене не будет — подтверждает ответ resume.
func TestOpenThreadAlreadyOnClaudexWorkIsDelivered(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	th := openThread(codex.WakePermissionProfile)
	th.ResumeSandbox = "workspaceWrite"
	app.Add(otherThread, th)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)

	res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if res.Delivered == nil || res.Delivered.Pending || res.Delivered.ThreadStage != codex.StageCompleted {
		t.Fatalf("ход начат и завершён: %+v", res.Delivered)
	}
	if app.Count("thread/settings/update") != 0 || app.Count("thread/queue/add") != 1 {
		t.Fatalf("update не нужен, сообщение одно: %v", app.Calls())
	}
}

func TestLoadedIdleThreadWithUnconfirmedProfileIsHeld(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	th := openThread(":workspace")
	th.FailSettings = true
	app.Add(otherThread, th)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)

	res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if res.Delivered == nil || !res.Delivered.Pending || res.Delivered.Cause != codex.HoldLoadedProfile ||
		res.Delivered.ThreadStage != codex.StageNotQueued {
		t.Fatalf("профиль не подтверждён — придержано до queue/add: %+v", res.Delivered)
	}
	Flush(context.Background(), FlushOptions{Journal: j})
	for _, m := range []string{"thread/queue/add", "thread/queue/start"} {
		if n := app.Count(m); n != 0 {
			t.Fatalf("%s не вызывался, получено %d: %v", m, n, app.Calls())
		}
	}
	if st := app.Thread(otherThread).Status.Type; st != codex.StatusIdle {
		t.Fatalf("тред остался idle, получено %q", st)
	}
	recs, _ := j.Read()
	if st := StateOf(recs, "465a77fc"); st.State == StateDone || st.State == StateQueued {
		t.Fatalf("непринятое не выдаётся ни за доставку, ни за очередь: %+v", st)
	}
}

// Наблюдатель дожимает неподтверждённый профиль: пока подтверждения нет —
// ни очереди, ни новых записей; как только есть — одно сообщение и один ход.
func TestUnconfirmedProfileIsRetriedBySupervisorWithoutDuplicate(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	th := openThread(":workspace")
	th.FailSettings = true
	app.Add(otherThread, th)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)
	stages := func() []string {
		recs, _ := j.Read()
		var out []string
		for _, r := range recs {
			if r.ThreadStage != "" {
				out = append(out, r.ThreadStage+"/"+r.Cause)
			}
		}
		return out
	}

	res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if res.Delivered == nil || !res.Delivered.Pending || res.Delivered.Cause != codex.HoldLoadedProfile {
		t.Fatalf("смена не подтверждена — придержано: %+v", res.Delivered)
	}
	for i := 0; i < 2; i++ {
		if f := Flush(context.Background(), FlushOptions{Journal: j}); len(f) != 1 || f[0].Ok {
			t.Fatalf("тик %d: без подтверждения не доставлено: %+v", i, f)
		}
	}
	if app.Count("thread/queue/add")+app.Count("thread/queue/start") != 0 {
		t.Fatalf("без подтверждения ничего не кладём: %v", app.Calls())
	}
	if got := stages(); len(got) != 1 || got[0] != codex.StageNotQueued+"/"+codex.HoldLoadedProfile {
		t.Fatalf("повторы с той же причиной журнал не множат: %v", got)
	}
	recs, _ := j.Read()
	if l := LostReports(recs); len(l) != 1 {
		t.Fatalf("отчёт не потерян, дожимается: %+v", l)
	}

	app.Set(otherThread, func(th *codextest.Thread) { th.FailSettings = false })
	if f := Flush(context.Background(), FlushOptions{Journal: j}); len(f) != 1 || !f[0].Ok {
		t.Fatalf("подтверждение пришло — доставлено: %+v", f)
	}
	Flush(context.Background(), FlushOptions{Journal: j})
	if app.Count("thread/queue/add") != 1 || len(app.Thread(otherThread).Turns) != 1 {
		t.Fatalf("одно сообщение, один ход: %v", app.Calls())
	}
	recs, _ = j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateDone {
		t.Fatalf("дошло, получено %+v", st)
	}
	if l := LostReports(recs); len(l) != 0 {
		t.Fatalf("дожимать больше нечего: %+v", l)
	}
	want := []string{codex.StageNotQueued + "/" + codex.HoldLoadedProfile, codex.StageStarted + "/", codex.StageCompleted + "/"}
	if got := stages(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("стадии по одной, получено %v", got)
	}
}

// Сервер применил профиль, но уведомление потерялось: следующий тик видит
// claudex-work в ответе resume и второй update не шлёт.
func TestLostSettingsNoticeIsConfirmedByResumeOnNextTick(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	th := openThread(":workspace")
	th.NoSettingsNotice = true
	th.ResumeSandbox = "workspaceWrite"
	app.Add(otherThread, th)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)

	res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if res.Delivered == nil || !res.Delivered.Pending || res.Delivered.Cause != codex.HoldLoadedProfile ||
		app.Count("thread/queue/add") != 0 {
		t.Fatalf("уведомления нет — придержано: %+v %v", res.Delivered, app.Calls())
	}
	if f := Flush(context.Background(), FlushOptions{Journal: j}); len(f) != 1 || !f[0].Ok {
		t.Fatalf("профиль назван resume — доставлено: %+v", f)
	}
	if app.Count("thread/settings/update") != 1 || app.Count("thread/queue/add") != 1 {
		t.Fatalf("один update, одно сообщение: %v", app.Calls())
	}
}

func TestFollowTurnsRecordsCompletionSeparately(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	app.Add(otherThread, &codextest.Thread{KeepRunning: true})
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", otherThread)

	res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	if res.Delivered.Pending || res.Delivered.ThreadStage != codex.StageStarted {
		t.Fatalf("ход начат, но не кончен: %+v", res.Delivered)
	}
	if got := FollowTurns(context.Background(), j, 3); len(got) != 0 {
		t.Fatalf("пока ход идёт, завершения нет: %+v", got)
	}
	app.Finish(otherThread, codex.TurnCompleted)
	got := FollowTurns(context.Background(), j, 3)
	if len(got) != 1 || got[0].Status != codex.TurnCompleted {
		t.Fatalf("завершение замечено, получено %+v", got)
	}
	if again := FollowTurns(context.Background(), j, 3); len(again) != 0 {
		t.Fatalf("завершение пишется один раз, получено %+v", again)
	}
	recs, _ := j.Read()
	lines := strings.Join(ThreadTimeline(recs, "465a77fc"), "\n")
	for _, want := range []string{"turn started", "turn completed", "статус хода: completed"} {
		if !strings.Contains(lines, want) {
			t.Errorf("в истории доставки есть %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "queue accepted") {
		t.Errorf("загрузка с ходом не выдумывает лишней стадии:\n%s", lines)
	}
}

func TestTransientAppServerFailureIsRetriedWithoutDuplicate(t *testing.T) {
	codexHome(t)
	app := codextest.Start(t)
	app.Add(otherThread, &codextest.Thread{})
	calls := 0
	prev := codex.Dial
	codex.Dial = func(ctx context.Context) (codex.AppServer, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("демон перезапускается")
		}
		return codex.DialSocket(ctx, app.Path)
	}
	t.Cleanup(func() { codex.Dial = prev })
	stub := 0
	prevQ := codex.Queue
	codex.Queue = func(context.Context, string, string) error { stub++; return errors.New("нет") }
	t.Cleanup(func() { codex.Queue = prevQ })

	_, j, c := threadDelivery(t)
	o := threadOpts()
	d := Deliver(context.Background(), c, j, "11111111", otherThread, "готово", o)
	if !d.OK || d.Pending || app.Count("thread/queue/add") != 1 {
		t.Fatalf("вторая попытка будит, сообщение одно: %+v %v", d, app.Calls())
	}
}
