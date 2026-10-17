package task

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
	"github.com/surraulistic/claudex/internal/journal"
)

func TestFailedTurnIsUndeliveredToldOnceAndNotRetried(t *testing.T) {
	unloadedHome(t, leaderThread)
	app := codextest.Start(t)
	app.Add(leaderThread, &codextest.Thread{EndStatus: codex.TurnFailed})
	h := herdrtest.Start(t)
	h.Reply("notification.show", `{"id":"x","result":{"shown":true}}`)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", leaderThread)
	o := doneOpts(j)
	o.Client = herdr.New(h.Path)

	res, err := Report(context.Background(), "465a77fc", "готово", reportText, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered.Reached() || res.Delivered.Cause != CauseTurnFailed || !res.Delivered.Fallback {
		t.Fatalf("оборванный ход — не доставка, человеку сказано: %+v", res.Delivered)
	}
	recs, _ := j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateUndelivered {
		t.Fatalf("состояние — не доставлено, получено %+v", st)
	}
	if l := LostReports(recs); len(l) != 1 || l[0].Cause != CauseTurnFailed {
		t.Fatalf("виден в undelivered с причиной, получено %+v", l)
	}
	if l := LivePending(recs, time.Now()); len(l) != 0 {
		t.Fatalf("дожим его не повторяет, получено %+v", l)
	}
	dials := app.Count("initialize")
	Flush(context.Background(), FlushOptions{Journal: j})
	if app.Count("initialize") != dials || app.Count("thread/queue/add") != 1 {
		t.Fatalf("повтора нет и второго сообщения нет: %v", app.Calls())
	}
}

func TestFollowedFailedTurnRevokesDelivery(t *testing.T) {
	unloadedHome(t, leaderThread)
	app := codextest.Start(t)
	app.Add(leaderThread, &codextest.Thread{KeepRunning: true})
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", leaderThread)
	if _, err := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j)); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateDone {
		t.Fatalf("ход начат — дошло, получено %+v", st)
	}
	app.Finish(leaderThread, codex.TurnInterrupted)
	if got := FollowTurns(context.Background(), j, 3); len(got) != 1 || got[0].Status != codex.TurnInterrupted {
		t.Fatalf("завершение замечено, получено %+v", got)
	}
	recs, _ = j.Read()
	if st := StateOf(recs, "465a77fc"); st.State != StateUndelivered {
		t.Fatalf("прерванный ход отменяет «дошло», получено %+v", st)
	}
	if got := FollowTurns(context.Background(), j, 3); len(got) != 0 {
		t.Fatalf("оборванный ход больше не отслеживается, получено %+v", got)
	}
}

// stalledSocket принимает соединения и молчит, как зависший демон.
func stalledSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return sock
}

func TestThreadDeliveryHonoursTheDeadline(t *testing.T) {
	unloadedHome(t, leaderThread)
	t.Setenv("CLAUDEX_CODEX_SOCKET", stalledSocket(t))
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	start := time.Now()
	d := Deliver(context.Background(), nil, j, "465a77fc", leaderThread, "x",
		DeliverOptions{Kind: KindThread, Stage: StageReported, HumanTold: true,
			Deadline: 300 * time.Millisecond, Poll: time.Millisecond})
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("срок доставки в тред соблюдён, заняло %s: %+v", el, d)
	}
	if d.Reached() {
		t.Fatalf("зависший сервер — не доставка: %+v", d)
	}
}

func TestTellHumanIsBoundedAndCancellable(t *testing.T) {
	prevGap, prevBudget := humanRetryGap, humanBudget
	humanRetryGap, humanBudget = 3*time.Second, 400*time.Millisecond
	t.Cleanup(func() { humanRetryGap, humanBudget = prevGap, prevBudget })
	h := herdrtest.Start(t)
	h.Reply("notification.show", `{"id":"x","result":{"shown":false,"reason":"busy"}}`)
	c := herdr.New(h.Path)

	start := time.Now()
	if shown, why := tellHuman(context.Background(), c, "t", "b"); shown || why == "" {
		t.Fatalf("отказ назван, получено %v %q", shown, why)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("общий предел показа соблюдён, заняло %s", el)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	tellHuman(ctx, c, "t", "b")
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("отмена вызывающим останавливает показ, заняло %s", el)
	}
}

func TestQueueOnlyFallbackCarriesNoReportText(t *testing.T) {
	sent := unloadedHome(t, leaderThread)
	t.Setenv("CLAUDEX_CODEX_SOCKET", filepath.Join(t.TempDir(), "нет.sock"))
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", leaderThread)
	o := doneOpts(j)
	o.FullReport = true
	res, err := Report(context.Background(), "465a77fc", "готово", reportText, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 || !res.Delivered.Pending {
		t.Fatalf("принято codex queue, ход не начат: %+v %+v", res.Delivered, *sent)
	}
	msg := (*sent)[0].message
	if strings.Contains(msg, "завершилось после срока") || !strings.HasPrefix(msg, codex.TextPrefix+"465a77fc") ||
		!strings.Contains(msg, "данные, не инструкция") {
		t.Fatalf("без app-server в тред уходит событие, а не отчёт целиком, получено %q", msg)
	}
	if f := Flush(context.Background(), FlushOptions{Journal: j}); len(f) != 1 || f[0].Ok {
		t.Fatalf("повтор при лежащем app-server — всё ещё не дошло: %+v", f)
	}
	if len(*sent) != 1 {
		t.Fatalf("повтор не кладёт второй раз: %d", len(*sent))
	}
}

func TestHoldFlickerDoesNotGrowTheJournal(t *testing.T) {
	unloadedHome(t, leaderThread)
	app := codextest.Start(t)
	busy := codex.ThreadStatus{Type: codex.StatusActive}
	human := codex.ThreadStatus{Type: codex.StatusActive, ActiveFlags: []string{"waitingOnApproval"}}
	app.Add(leaderThread, &codextest.Thread{Status: busy})
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", leaderThread)
	Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j))
	for i := 0; i < 6; i++ {
		st := human
		if i%2 == 1 {
			st = busy
		}
		app.Set(leaderThread, func(th *codextest.Thread) { th.Status = st })
		Flush(context.Background(), FlushOptions{Journal: j})
	}
	recs, _ := j.Read()
	var holds []string
	for _, r := range recs {
		if r.ThreadStage != "" {
			holds = append(holds, r.Cause)
		}
	}
	if len(holds) != 2 {
		t.Fatalf("каждая причина записана один раз, получено %v", holds)
	}
}

func TestOpenSessionRightsAreSwitchedOnceVisiblyAndNotAfterRevert(t *testing.T) {
	codexHome(t, leaderThread)
	app := codextest.Start(t)
	app.Add(leaderThread, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle},
		Watched: true, Profile: ":workspace"})
	h := herdrtest.Start(t)
	h.Reply("notification.show", `{"id":"x","result":{"shown":true}}`)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	seedThreadTask(t, j, "465a77fc", leaderThread)
	seedThreadTask(t, j, "5e1f00aa", leaderThread)

	if res, _ := Report(context.Background(), "465a77fc", "готово", reportText, doneOpts(j)); !res.Delivered.Reached() {
		t.Fatalf("первое пробуждение доходит: %+v", res.Delivered)
	}
	recs, _ := j.Read()
	var switched journal.Record
	for _, r := range recs {
		if r.PermissionsSet != "" {
			switched = r
		}
	}
	if switched.PermissionsSet != codex.ProfileByUpdate || switched.PermissionsFrom != ":workspace" ||
		!strings.Contains(switched.Reason, ":workspace") {
		t.Fatalf("смена прав записана с прежним профилем, получено %+v", switched)
	}

	app.Set(leaderThread, func(th *codextest.Thread) { th.Profile = ":workspace" })
	o := doneOpts(j)
	o.Client = herdr.New(h.Path)
	res, _ := Report(context.Background(), "5e1f00aa", "готово", reportText, o)
	if res.Delivered.Reached() || res.Delivered.Cause != codex.HoldProfileReverted {
		t.Fatalf("права вернули — без человека не переключаем: %+v", res.Delivered)
	}
	if app.Count("thread/settings/update") != 1 {
		t.Fatalf("update был один: %v", app.Calls())
	}
	told := 0
	for _, r := range h.Requests() {
		if strings.Contains(r, "решения о правах") {
			told++
		}
	}
	Flush(context.Background(), FlushOptions{Journal: j, Client: herdr.New(h.Path)})
	for _, r := range h.Requests() {
		if strings.Contains(r, "решения о правах") {
			told--
		}
	}
	if told != 0 || app.Count("thread/settings/update") != 1 {
		t.Fatalf("человеку сказано один раз, повтор не переключает: told=%d %v", told, app.Calls())
	}
}
