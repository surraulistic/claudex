package task

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
	"github.com/surraulistic/claudex/internal/journal"
)

func agent(status string) string {
	return `{"id":"x","result":{"type":"agent","agent":{"pane_id":"wE:p2","agent":"claude","agent_status":"` + status + `"}}}`
}

func setup(t *testing.T, status string) (*herdrtest.Fake, *journal.Journal, Options) {
	t.Helper()
	f := herdrtest.Start(t)
	f.Reply("agent.get", agent(status))
	f.Reply("agent.prompt", agent("idle"))
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	return f, j, Options{
		Client: herdr.New(f.Path), Journal: j,
		Pane: "wE:p2", Prompt: "почини сборку",
		Timeout: 3 * time.Second, Grace: 150 * time.Millisecond,
		Self: "/путь/к/claudex",
	}
}

func TestBusyPaneIsRefused(t *testing.T) {
	// Занятой панели задание не шлётся: оно уедет в чужой разговор.
	_, _, o := setup(t, "working")
	_, err := Delegate(context.Background(), o)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("занятая панель отклоняется, получено %v", err)
	}
}

func TestPromptCarriesReportInstructionWithTaskID(t *testing.T) {
	f, _, o := setup(t, "idle")
	go Delegate(context.Background(), o)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if req := f.LastRequest("agent.prompt"); req != nil {
			text := req["params"].(map[string]any)["text"].(string)
			if !strings.Contains(text, "почини сборку") {
				t.Fatalf("задание сохранено целиком, получено %q", text)
			}
			// Отчитываться нужно тем же бинарём, что делегирует: на PATH
			// может лежать другая сборка, которая про `done` не знает.
			if !strings.Contains(text, "/путь/к/claudex done ") {
				t.Fatalf("в задание вписан отчёт своим бинарём, получено %q", text)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("задание не ушло")
}

func TestReportedOutcomeIsCorrelated(t *testing.T) {
	// Отчёт приходит по журналу с идентификатором задачи — это и есть
	// доказательство, что завершилась именно посланная задача.
	f, j, o := setup(t, "idle")
	f.Delay("agent.prompt", 2*time.Second)
	go func() {
		id := waitForTaskID(t, j)
		j.Append(journal.Record{Task: id, Event: journal.Reported,
			Pane: "wE:p2", Outcome: "готово", Reason: "сборка зелёная"})
	}()
	res, err := Delegate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Reported || res.Said != "готово" || res.Reason != "сборка зелёная" {
		t.Fatalf("своя классификация и слово задачи врозь, получено %+v", res)
	}
}

func TestSilentFinishIsMarkedApart(t *testing.T) {
	// Панель освободилась, но не отчиталась: это не то же самое, что «готово»,
	// и вести себя как «готово» не должно.
	_, _, o := setup(t, "idle")
	res, err := Delegate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Silent {
		t.Fatalf("молчаливое завершение отличается от отчёта, получено %q", res.Outcome)
	}
}

func TestTimeoutIsItsOwnOutcome(t *testing.T) {
	f, _, o := setup(t, "idle")
	f.Delay("agent.prompt", 2*time.Second)
	o.Timeout = 120 * time.Millisecond
	res, _ := Delegate(context.Background(), o)
	if res.Outcome != TimedOut {
		t.Fatalf("срок — отдельный исход, получено %q", res.Outcome)
	}
}

func TestHerdrFailureDuringWaitIsNotATimeout(t *testing.T) {
	// «Не смогли узнать» — не «не завершилось». Договор требует различать их
	// кодами 7 и 5, и раньше любой отказ herdr становился сроком.
	f, _, o := setup(t, "idle")
	f.Reply("agent.prompt", `{"id":"x","error":{"code":"internal_error","message":"herdr прилёг"}}`)
	res, err := Delegate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Unknown {
		t.Fatalf("отказ herdr — отдельный исход, получено %q", res.Outcome)
	}

	f2, _, o2 := setup(t, "idle")
	f2.Reply("agent.prompt", `{"id":"x","error":{"code":"timeout","message":"не дождались"}}`)
	res2, _ := Delegate(context.Background(), o2)
	if res2.Outcome != TimedOut {
		t.Fatalf("срок остаётся сроком, получено %q", res2.Outcome)
	}
}

func TestFreeMatchesDelegateRefusal(t *testing.T) {
	// Проверку состояния зовут из двух мест; расходиться им нельзя.
	for status, want := range map[string]bool{
		"idle": true, "done": true, "blocked": false, "unknown": false, "working": false,
	} {
		if Free(status) != want {
			t.Fatalf("Free(%q) = %v", status, Free(status))
		}
	}
}

func TestJournalKeepsStartAndFinish(t *testing.T) {
	_, j, o := setup(t, "idle")
	res, err := Delegate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	var started, finished bool
	for _, r := range recs {
		if r.Task != res.Task {
			t.Fatalf("записи чужой задачи: %+v", r)
		}
		switch r.Event {
		case journal.Started:
			started = true
			if r.Prompt == "" || r.Pane == "" {
				t.Fatalf("начало записано с заданием и панелью, получено %+v", r)
			}
		case journal.Finished:
			finished = true
			if r.Outcome != res.Outcome {
				t.Fatalf("исход в журнале и в ответе совпадают, получено %q и %q", r.Outcome, res.Outcome)
			}
		}
	}
	if !started || !finished {
		t.Fatalf("начало и конец записаны, получено %+v", recs)
	}
}

func TestReportUsesPaneFromEnvironment(t *testing.T) {
	// Отчитывается чужой процесс внутри панели; свою панель он знает из
	// HERDR_PANE_ID, который herdr кладёт в окружение каждой панели.
	_, j, _ := setup(t, "idle")
	t.Setenv("HERDR_PANE_ID", "wE:p7")
	if err := Report(j, "т1", "готово", "всё сделано"); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	if len(recs) != 1 || recs[0].Pane != "wE:p7" || recs[0].Event != journal.Reported {
		t.Fatalf("отчёт с панелью из окружения, получено %+v", recs)
	}
}

func waitForTaskID(t *testing.T, j *journal.Journal) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recs, _ := j.Read(); len(recs) > 0 {
			return recs[0].Task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("задача не появилась в журнале")
	return ""
}

var _ = json.Marshal

func delivery(t *testing.T, status string) (*herdrtest.Fake, *journal.Journal, *herdr.Client) {
	t.Helper()
	f := herdrtest.Start(t)
	f.Reply("agent.get", agent(status))
	f.Reply("agent.prompt", agent("idle"))
	f.Reply("notification.show", `{"id":"x","result":{"shown":true}}`)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	return f, j, herdr.New(f.Path)
}

func TestDeliverWaitsForTheLeaderInsteadOfGivingUp(t *testing.T) {
	// Ровно тот сбой: ведущий был занят дольше пятнадцати секунд, и
	// пробуждение не случилось вовсе. Ожидание живёт в уже запущенном
	// наблюдателе и стоит только опроса herdr.
	f, j, c := delivery(t, "working")
	go func() {
		time.Sleep(80 * time.Millisecond)
		f.Reply("agent.get", agent("idle"))
	}()
	d := Deliver(context.Background(), c, j, "т1", "wE:p17", "готово",
		DeliverOptions{Deadline: 3 * time.Second, Poll: 10 * time.Millisecond})
	if !d.OK {
		t.Fatalf("дождались и разбудили, получено %+v", d)
	}
	if f.LastRequest("agent.prompt") == nil {
		t.Fatal("сообщение ушло ведущему")
	}
}

func TestDeliverFallsBackToTheHumanWhenLeaderStaysBusy(t *testing.T) {
	// Ограничение вне claudex: ведущий может быть занят сколько угодно. Тогда
	// факт уходит человеку уведомлением herdr — мимо агентов и без токенов.
	f, j, c := delivery(t, "working")
	d := Deliver(context.Background(), c, j, "т1", "wE:p17", "поручение готово",
		DeliverOptions{Deadline: 60 * time.Millisecond, Poll: 10 * time.Millisecond})
	if d.OK {
		t.Fatal("ведущий так и не освободился")
	}
	if !d.Fallback {
		t.Fatalf("человеку показано, получено %+v", d)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("в занятую панель ничего не отправлено")
	}
	req := f.LastRequest("notification.show")
	if req == nil {
		t.Fatal("уведомление человеку не ушло")
	}
	body, _ := req["params"].(map[string]any)["body"].(string)
	if !strings.Contains(body, "поручение готово") {
		t.Fatalf("в уведомлении сам результат, получено %q", body)
	}
}

func TestDeliveryOutcomeLandsInTheJournal(t *testing.T) {
	// Прежде итог пробуждения жил только в логе отсоединённого наблюдателя,
	// который никто не читает, — поэтому сбой и остался незамеченным.
	for _, c := range []struct {
		status, want string
		deadline     time.Duration
	}{
		{"idle", "разбужен", time.Second},
		{"working", "не разбужен, показано человеку", 40 * time.Millisecond},
	} {
		_, j, cl := delivery(t, c.status)
		Deliver(context.Background(), cl, j, "т1", "wE:p17", "готово",
			DeliverOptions{Deadline: c.deadline, Poll: 10 * time.Millisecond})
		recs, _ := j.Read()
		var got *journal.Record
		for i := range recs {
			if recs[i].Event == journal.Notified {
				got = &recs[i]
			}
		}
		if got == nil {
			t.Fatalf("%s: записи о пробуждении нет", c.status)
		}
		if got.Outcome != c.want || got.Target != "wE:p17" {
			t.Fatalf("%s: получено %+v", c.status, *got)
		}
	}
}

func TestDeliverStopsOnContextCancel(t *testing.T) {
	_, j, c := delivery(t, "working")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := Deliver(ctx, c, j, "т1", "wE:p17", "готово",
		DeliverOptions{Deadline: time.Hour, Poll: 10 * time.Millisecond})
	if d.OK {
		t.Fatal("отменённая доставка не считается удавшейся")
	}
}
