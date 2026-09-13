package task

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	if _, err := Report(context.Background(), "т1", "готово", "всё сделано",
		ReportOptions{Journal: j}); err != nil {
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

// Поручение 6dea9f33: наблюдатель дошёл до срока в 18:45, записал
// «не уложилась в срок» и разбудил ведущего, задача продолжила работу и
// отчиталась в 18:59 — этот отчёт не доходил никуда, потому что наблюдателя
// уже не было.
func timedOut(t *testing.T, j *journal.Journal, target string) {
	t.Helper()
	j.Append(journal.Record{Task: "т1", Event: journal.Started, Pane: "wE:p13", Target: target})
	j.Append(journal.Record{Task: "т1", Event: journal.Finished, Pane: "wE:p13",
		Outcome: TimedOut, Reason: "context deadline exceeded"})
	j.Append(journal.Record{Task: "т1", Event: journal.Notified, Target: target,
		Stage: StageFinished, Outcome: "разбужен"})
}

func TestLateReportWakesTheLeaderTheWatcherCouldNotWaitFor(t *testing.T) {
	f, j, c := delivery(t, "idle")
	timedOut(t, j, "wE:p17")

	res, err := Report(context.Background(), "т1", "готово", "каталог опубликован",
		ReportOptions{Client: c, Journal: j, Deadline: time.Second, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Late || res.Delivered == nil || !res.Delivered.OK {
		t.Fatalf("поздний отчёт доставлен ведущему, получено %+v", res)
	}
	body, _ := f.LastRequest("agent.prompt")["params"].(map[string]any)["text"].(string)
	if !strings.Contains(body, "каталог опубликован") {
		t.Fatalf("ведущему уехал сам отчёт, получено %q", body)
	}
}

func TestLateReportFallsBackToHumanWhenLeaderIsBusy(t *testing.T) {
	// done выполняется внутри хода самой задачи, поэтому ждать долго нельзя:
	// занят ведущий — говорим человеку сразу.
	f, j, c := delivery(t, "working")
	timedOut(t, j, "wE:p17")

	res, _ := Report(context.Background(), "т1", "готово", "каталог опубликован",
		ReportOptions{Client: c, Journal: j, Deadline: 40 * time.Millisecond, Poll: 10 * time.Millisecond})
	if res.Delivered == nil || res.Delivered.OK || !res.Delivered.Fallback {
		t.Fatalf("человеку показано, получено %+v", res.Delivered)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("в занятую панель ничего не отправлено")
	}
	body, _ := f.LastRequest("notification.show")["params"].(map[string]any)["body"].(string)
	if !strings.Contains(body, "каталог опубликован") {
		t.Fatalf("в уведомлении сам отчёт, получено %q", body)
	}
}

func TestRepeatedDoneDoesNotWakeTheLeaderTwice(t *testing.T) {
	f, j, c := delivery(t, "idle")
	timedOut(t, j, "wE:p17")
	o := ReportOptions{Client: c, Journal: j, Deadline: time.Second, Poll: 10 * time.Millisecond}

	if res, _ := Report(context.Background(), "т1", "готово", "первый", o); !res.Late {
		t.Fatal("первый поздний отчёт доставляется")
	}
	prompts := len(f.Requests())
	res, _ := Report(context.Background(), "т1", "готово", "второй", o)
	if res.Late || res.Skipped == "" {
		t.Fatalf("повтор не будит второй раз, получено %+v", res)
	}
	if len(f.Requests()) != prompts {
		t.Fatal("повтор не ходил в herdr вовсе")
	}
	// Сам отчёт при этом записан: журнал — история, а не состояние.
	recs, _ := j.Read()
	var reported int
	for _, r := range recs {
		if r.Event == journal.Reported {
			reported++
		}
	}
	if reported != 2 {
		t.Fatalf("оба вызова done записаны, получено %d", reported)
	}
}

func TestFailedDeliveryIsRetriedOnTheNextDone(t *testing.T) {
	// Если не достучались ни до кого, повторить стоит — в отличие от удачи.
	f, j, c := delivery(t, "working")
	f.Reply("notification.show", `{"id":"x","error":{"code":"boom","message":"нет"}}`)
	timedOut(t, j, "wE:p17")
	o := ReportOptions{Client: c, Journal: j, Deadline: 30 * time.Millisecond, Poll: 10 * time.Millisecond}

	if res, _ := Report(context.Background(), "т1", "готово", "первый", o); res.Delivered.Fallback {
		t.Fatal("человеку показать не удалось")
	}
	f.Reply("agent.get", agent("idle"))
	res, _ := Report(context.Background(), "т1", "готово", "второй", o)
	if !res.Late || res.Delivered == nil || !res.Delivered.OK {
		t.Fatalf("недоставленное повторяется, получено %+v", res)
	}
}

func TestReportWhileWatcherAliveDoesNotDeliverItself(t *testing.T) {
	// Наблюдатель ещё ждёт отчёт через журнал — вторая доставка была бы дублем.
	f, j, c := delivery(t, "idle")
	j.Append(journal.Record{Task: "т1", Event: journal.Started, Pane: "wE:p13", Target: "wE:p17"})

	res, _ := Report(context.Background(), "т1", "готово", "рано",
		ReportOptions{Client: c, Journal: j, Deadline: time.Second})
	if res.Late || res.Delivered != nil {
		t.Fatalf("живой наблюдатель доставляет сам, получено %+v", res)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("в herdr никто не ходил")
	}
}

func TestLateReportWithoutLeaderIsNotSpam(t *testing.T) {
	f, j, c := delivery(t, "idle")
	timedOut(t, j, "")
	res, _ := Report(context.Background(), "т1", "готово", "некому",
		ReportOptions{Client: c, Journal: j, Deadline: time.Second})
	if res.Late || res.Skipped == "" {
		t.Fatalf("без ведущего доставлять некому, получено %+v", res)
	}
	if len(f.Requests()) != 0 {
		t.Fatal("и в herdr не ходили")
	}
}

func agentWith(status, session string) string {
	return `{"id":"x","result":{"type":"agent","agent":{"pane_id":"wE:p17","agent":"codex",` +
		`"agent_status":"` + status + `","agent_session":{"value":"` + session + `"}}}}`
}

// Панель переживает смену агента, разговор в ней — нет. Будить надо тот, что
// поручение затеял, иначе сообщение о чужой задаче уедет человеку, который её
// не посылал.
func TestWakeGoesToTheSessionThatStartedItNotJustThePane(t *testing.T) {
	f, j, c := delivery(t, "idle")
	f.Reply("agent.get", agentWith("idle", "разговор-А"))

	d := Deliver(context.Background(), c, j, "т1", "wE:p17", "готово",
		DeliverOptions{Stage: StageReported, WantSession: "разговор-А",
			Deadline: time.Second, Poll: 10 * time.Millisecond})
	if !d.OK {
		t.Fatalf("тот же разговор — будим, получено %+v", d)
	}
}

func TestWakeRefusesAPaneWhoseConversationChanged(t *testing.T) {
	f, j, c := delivery(t, "idle")
	f.Reply("agent.get", agentWith("idle", "разговор-Б"))

	d := Deliver(context.Background(), c, j, "т1", "wE:p17", "готово",
		DeliverOptions{Stage: StageReported, WantSession: "разговор-А",
			Deadline: time.Second, Poll: 10 * time.Millisecond})
	if d.OK {
		t.Fatal("в чужой разговор писать нельзя")
	}
	if !d.Fallback {
		t.Fatalf("вместо этого говорим человеку, получено %+v", d)
	}
	if !strings.Contains(d.Reason, "другой разговор") {
		t.Fatalf("причина названа, получено %q", d.Reason)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("в панель не ушло ничего")
	}
	recs, _ := j.Read()
	if len(recs) != 1 || recs[0].Outcome != "не разбужен, показано человеку" {
		t.Fatalf("отказ виден в журнале, получено %+v", recs)
	}
}

func TestWakeProceedsWhenThereIsNothingToCompare(t *testing.T) {
	// Привязки может не быть: герой не всегда сообщает разговор. Тогда
	// проверять нечего, и отказывать не за что.
	for _, c := range []struct{ want, has string }{
		{"", "разговор-Б"}, // не записали при заведении
		{"разговор-А", ""}, // герой не сообщает сейчас
	} {
		f, j, cl := delivery(t, "idle")
		f.Reply("agent.get", agentWith("idle", c.has))
		d := Deliver(context.Background(), cl, j, "т1", "wE:p17", "готово",
			DeliverOptions{Stage: StageReported, WantSession: c.want,
				Deadline: time.Second, Poll: 10 * time.Millisecond})
		if !d.OK {
			t.Fatalf("want=%q has=%q: получено %+v", c.want, c.has, d)
		}
	}
}

func TestLateReportKeepsTheBindingFromDelegationTime(t *testing.T) {
	// Поздний отчёт приходит спустя час; за это время панель могла сменить
	// разговор, и привязка нужна именно та, что снята при заведении.
	f, j, c := delivery(t, "idle")
	f.Reply("agent.get", agentWith("idle", "разговор-Б"))
	j.Append(journal.Record{Task: "т1", Event: journal.Started, Pane: "wE:p13",
		Target: "wE:p17", TargetSession: "разговор-А"})
	j.Append(journal.Record{Task: "т1", Event: journal.Finished, Outcome: TimedOut})

	res, _ := Report(context.Background(), "т1", "готово", "поздний отчёт",
		ReportOptions{Client: c, Journal: j, Deadline: time.Second, Poll: 10 * time.Millisecond})
	if res.Delivered == nil || res.Delivered.OK || !res.Delivered.Fallback {
		t.Fatalf("чужому разговору не пишем, человеку говорим: %+v", res.Delivered)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("в панель не ушло ничего")
	}
}

func TestShortCutsWholeCharacters(t *testing.T) {
	// Байтовый срез рубил кириллическую букву пополам, и в причине отказа
	// оказывался мусор вместо имени разговора.
	got := short("Ярославна-длинное-имя")
	if !utf8.ValidString(got) {
		t.Fatalf("срез оставляет целые буквы, получено %q", got)
	}
	if r := []rune(got); len(r) != 9 || string(r[:8]) != "Ярославн" {
		t.Fatalf("получено %q", got)
	}
	if a, b := short("7e403273-2034-4296"), short("01a07921-5154-7980"); a == b {
		t.Fatal("разные идентификаторы остаются различимы")
	}
	if got := short("коротко"); got != "коротко" {
		t.Fatalf("короткое не трогается, получено %q", got)
	}
}
