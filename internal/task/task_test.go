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
	j.Append(journal.Record{Task: "11111111", Event: journal.Started, Pane: "wE:p7"})
	if _, err := Report(context.Background(), "11111111", "готово", "всё сделано",
		ReportOptions{Journal: j}); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.Read()
	if len(recs) != 2 || recs[1].Pane != "wE:p7" || recs[1].Event != journal.Reported {
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

// ведущий, чей разговор известен: доставка без подтверждённой привязки
// запрещена, поэтому у опоры тестов он есть.
const leaderSession = "01a07921-5154-7980-abd5-945161e3ea08"

func delivery(t *testing.T, status string) (*herdrtest.Fake, *journal.Journal, *herdr.Client) {
	t.Helper()
	f := herdrtest.Start(t)
	f.Reply("agent.get", agentWith(status, leaderSession))
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
		f.Reply("agent.get", agentWith("idle", leaderSession))
	}()
	d := Deliver(context.Background(), c, j, "11111111", "wE:p17", "готово",
		DeliverOptions{WantSession: leaderSession, Deadline: 3 * time.Second, Poll: 10 * time.Millisecond})
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
	d := Deliver(context.Background(), c, j, "11111111", "wE:p17", "поручение готово",
		DeliverOptions{WantSession: leaderSession, Deadline: 60 * time.Millisecond, Poll: 10 * time.Millisecond})
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
		Deliver(context.Background(), cl, j, "11111111", "wE:p17", "готово",
			DeliverOptions{WantSession: leaderSession, Deadline: c.deadline, Poll: 10 * time.Millisecond})
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
	d := Deliver(ctx, c, j, "11111111", "wE:p17", "готово",
		DeliverOptions{WantSession: leaderSession, Deadline: time.Hour, Poll: 10 * time.Millisecond})
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
	j.Append(journal.Record{Task: "11111111", Event: journal.Started, Pane: "wE:p13",
		Target: target, TargetSession: leaderSession})
	j.Append(journal.Record{Task: "11111111", Event: journal.Finished, Pane: "wE:p13",
		Outcome: TimedOut, Reason: "context deadline exceeded"})
	j.Append(journal.Record{Task: "11111111", Event: journal.Notified, Target: target,
		Stage: StageFinished, Outcome: "разбужен"})
}

func TestLateReportWakesTheLeaderTheWatcherCouldNotWaitFor(t *testing.T) {
	f, j, c := delivery(t, "idle")
	timedOut(t, j, "wE:p17")

	res, err := Report(context.Background(), "11111111", "готово", "каталог опубликован",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second, Poll: 10 * time.Millisecond})
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

	res, _ := Report(context.Background(), "11111111", "готово", "каталог опубликован",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: 40 * time.Millisecond, Poll: 10 * time.Millisecond})
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
	o := ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second, Poll: 10 * time.Millisecond}

	if res, _ := Report(context.Background(), "11111111", "готово", "первый", o); !res.Late {
		t.Fatal("первый поздний отчёт доставляется")
	}
	prompts := len(f.Requests())
	res, _ := Report(context.Background(), "11111111", "готово", "второй", o)
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
	o := ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: 30 * time.Millisecond, Poll: 10 * time.Millisecond}

	if res, _ := Report(context.Background(), "11111111", "готово", "первый", o); res.Delivered.Fallback {
		t.Fatal("человеку показать не удалось")
	}
	f.Reply("agent.get", agentWith("idle", leaderSession))
	res, _ := Report(context.Background(), "11111111", "готово", "второй", o)
	if !res.Late || res.Delivered == nil || !res.Delivered.OK {
		t.Fatalf("недоставленное повторяется, получено %+v", res)
	}
}

func TestReportWhileWatcherAliveDoesNotDeliverItself(t *testing.T) {
	// Наблюдатель ещё ждёт отчёт через журнал — вторая доставка была бы дублем.
	f, j, c := delivery(t, "idle")
	j.Append(journal.Record{Task: "11111111", Event: journal.Started, Pane: "wE:p13",
		Target: "wE:p17", TargetSession: leaderSession})

	res, _ := Report(context.Background(), "11111111", "готово", "рано",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second})
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
	res, _ := Report(context.Background(), "11111111", "готово", "некому",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second})
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

	d := Deliver(context.Background(), c, j, "11111111", "wE:p17", "готово",
		DeliverOptions{Stage: StageReported, WantSession: "разговор-А",
			Deadline: time.Second, Poll: 10 * time.Millisecond})
	if !d.OK {
		t.Fatalf("тот же разговор — будим, получено %+v", d)
	}
}

func TestWakeRefusesAPaneWhoseConversationChanged(t *testing.T) {
	f, j, c := delivery(t, "idle")
	f.Reply("agent.get", agentWith("idle", "разговор-Б"))

	d := Deliver(context.Background(), c, j, "11111111", "wE:p17", "готово",
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

func TestWakeRefusesWhenTheBindingCannotBeConfirmed(t *testing.T) {
	// Рядом живут другие сессии того же ведущего. Неизвестный идентификатор —
	// это «подтвердить нечем», а не «наверное, тот же»: прежнее мягкое правило
	// писало в панель что угодно, стоило герою не сообщить разговор.
	for _, c := range []struct{ name, want, has string }{
		{"не записали при заведении", "", leaderSession},
		{"герой не сообщает сейчас", leaderSession, ""},
		{"неизвестно с обеих сторон", "", ""},
	} {
		f, j, cl := delivery(t, "idle")
		f.Reply("agent.get", agentWith("idle", c.has))
		d := Deliver(context.Background(), cl, j, "11111111", "wE:p17", "готово",
			DeliverOptions{Stage: StageReported, WantSession: c.want,
				Deadline: time.Second, Poll: 10 * time.Millisecond})
		if d.OK {
			t.Fatalf("%s: в панель писать нельзя", c.name)
		}
		if !d.Fallback {
			t.Fatalf("%s: вместо этого говорим человеку, получено %+v", c.name, d)
		}
		if !strings.Contains(d.Reason, "нечем") {
			t.Fatalf("%s: причина названа, получено %q", c.name, d.Reason)
		}
		if f.LastRequest("agent.prompt") != nil {
			t.Fatalf("%s: в herdr не ходили с промптом", c.name)
		}
	}
}

func TestLateReportKeepsTheBindingFromDelegationTime(t *testing.T) {
	// Поздний отчёт приходит спустя час; за это время панель могла сменить
	// разговор, и привязка нужна именно та, что снята при заведении.
	f, j, c := delivery(t, "idle")
	f.Reply("agent.get", agentWith("idle", "разговор-Б"))
	j.Append(journal.Record{Task: "11111111", Event: journal.Started, Pane: "wE:p13",
		Target: "wE:p17", TargetSession: "разговор-А"})
	j.Append(journal.Record{Task: "11111111", Event: journal.Finished, Outcome: TimedOut})

	res, _ := Report(context.Background(), "11111111", "готово", "поздний отчёт",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second, Poll: 10 * time.Millisecond})
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

func init() { humanRetryGap = time.Millisecond }

func TestRefusedNotificationIsNotCalledDelivered(t *testing.T) {
	// herdr отвечает успехом и при отказе показать. Прежде claudex писал
	// «показано человеку», хотя человек не видел ничего.
	f, j, c := delivery(t, "working")
	f.Reply("notification.show",
		`{"id":"x","result":{"type":"notification_show","shown":false,"reason":"busy"}}`)

	d := Deliver(context.Background(), c, j, "11111111", "wE:p17", "готово",
		DeliverOptions{Stage: StageFinished, WantSession: leaderSession,
			Deadline: 30 * time.Millisecond, Poll: 10 * time.Millisecond})
	if d.Fallback {
		t.Fatal("отказ показать — не показ")
	}
	if d.Refused != "busy" {
		t.Fatalf("причина отказа сохранена, получено %q", d.Refused)
	}
	recs, _ := j.Read()
	if !strings.HasPrefix(recs[0].Outcome, NotDelivered) || !strings.Contains(recs[0].Outcome, "busy") {
		t.Fatalf("журнал говорит правду, получено %q", recs[0].Outcome)
	}
}

func TestTransientRefusalIsRetried(t *testing.T) {
	// Отказ busy наблюдался преходящим: то же уведомление показывалось позже.
	f, j, c := delivery(t, "working")
	f.Reply("notification.show",
		`{"id":"x","result":{"shown":false,"reason":"busy"}}`)
	go func() {
		time.Sleep(2 * time.Millisecond)
		f.Reply("notification.show", `{"id":"x","result":{"shown":true,"reason":"shown"}}`)
	}()
	d := Deliver(context.Background(), c, j, "11111111", "wE:p17", "готово",
		DeliverOptions{Stage: StageFinished, WantSession: leaderSession,
			Deadline: 20 * time.Millisecond, Poll: 5 * time.Millisecond})
	if !d.Fallback {
		t.Fatalf("повтор доносит, получено %+v", d)
	}
}

func TestUndeliveredHoldsWhatTheLeaderNeverGot(t *testing.T) {
	// Смысл канала — «ведущий не получил», а не «не узнал никто». Показ
	// человеку отчёт не доставляет: всплывашку прочитают, когда будут за
	// машиной, а разговор так и не узнает, что поручение кончилось.
	now := time.Now()
	recs := []journal.Record{
		{Task: "11111111", Time: now, Event: journal.Reported, Outcome: "готово", Reason: "карта флота готова"},
		{Task: "11111111", Time: now, Event: journal.Notified, Stage: StageFinished, Target: "wE:p17",
			Outcome: NotDelivered + " (herdr: busy)", Reason: "теперь другой разговор"},
		{Task: "т2", Time: now, Event: journal.Notified, Stage: StageFinished, Outcome: ToldHuman},
		{Task: "т3", Time: now, Event: journal.Notified, Stage: StageFinished, Outcome: WokeUp},
	}
	got := LostReports(recs)
	if len(got) != 2 {
		t.Fatalf("не дошло до ведущего две, получено %+v", got)
	}
	if got[0].Task != "11111111" || got[0].Stage != StageFinished || got[0].Target != "wE:p17" {
		t.Fatalf("получено %+v", got[0])
	}
	if got[1].Task != "т2" {
		t.Fatalf("показ человеку остаётся недоставленным, получено %+v", got[1])
	}
	if !strings.Contains(got[0].Report, "карта флота готова") {
		t.Fatalf("сам отчёт при потере не теряется, получено %q", got[0].Report)
	}
	if got[0].At == "" || !strings.Contains(got[0].Reason, "busy") {
		t.Fatalf("когда и почему, получено %+v", got[0])
	}
}

func TestLaterSuccessClearsTheLoss(t *testing.T) {
	now := time.Now()
	recs := []journal.Record{
		{Task: "11111111", Time: now, Event: journal.Notified, Stage: StageFinished, Outcome: NotDelivered},
		{Task: "11111111", Time: now, Event: journal.Notified, Stage: StageFinished, Outcome: WokeUp},
	}
	if got := LostReports(recs); len(got) != 0 {
		t.Fatalf("дошедшее позже перестаёт быть потерей, получено %+v", got)
	}
}

func TestLostReportsSeparatesStages(t *testing.T) {
	now := time.Now()
	recs := []journal.Record{
		{Task: "11111111", Time: now, Event: journal.Notified, Stage: StageFinished, Outcome: WokeUp},
		{Task: "11111111", Time: now, Event: journal.Notified, Stage: StageReported, Outcome: NotDelivered},
	}
	got := LostReports(recs)
	if len(got) != 1 || got[0].Stage != StageReported {
		t.Fatalf("доставленная стадия не скрывает потерянную, получено %+v", got)
	}
}

func TestDelegateRecordsWhatTheWakeIsBoundTo(t *testing.T) {
	// Привязку снимают при заведении поручения, а не при доставке: к моменту
	// доставки в панели может быть уже другая сессия.
	f, j, o := setup(t, "idle")
	f.Reply("agent.get", agentWith("idle", leaderSession))
	o.Notify = "wE:p17"

	res, err := Delegate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.WakeTarget != "wE:p17" || res.WakeSession != leaderSession {
		t.Fatalf("цель и разговор запомнены, получено %+v", res)
	}
	recs, _ := j.Read()
	if recs[0].Event != journal.Started || recs[0].TargetSession != leaderSession {
		t.Fatalf("привязка попала в журнал, получено %+v", recs[0])
	}
}

func TestDelegateWithoutReachableLeaderLeavesTheBindingEmpty(t *testing.T) {
	// Герой не сообщает разговор — привязки нет, и это видно сразу, а не
	// через полчаса на попытке разбудить.
	f, _, o := setup(t, "idle")
	f.Reply("agent.get", agent("idle"))
	o.Notify = "wE:p17"

	res, _ := Delegate(context.Background(), o)
	if res.WakeSession != "" {
		t.Fatalf("привязки нет, получено %q", res.WakeSession)
	}
}

// Одна панель Codex ведёт несколько разговоров разом: измерено, что herdr
// показывает у wE:p17 один идентификатор, пока Codex пишет три файла — два
// лицензионных и один по багам. Совпадение идентификатора не доказывает, что
// слушает именно тот разговор, поэтому такая панель адресом не считается.
func journalWithTwoConversations(t *testing.T, j *journal.Journal) {
	t.Helper()
	j.Append(journal.Record{Task: "лиценз", Event: journal.Started, Pane: "wE:p13",
		Target: "wE:p17", TargetSession: "01a07921"})
	j.Append(journal.Record{Task: "ba614444", Event: journal.Started, Pane: "wE:p2N",
		Target: "wE:p17", TargetSession: leaderSession})
}

func TestSharedPaneIsNotAnAddress(t *testing.T) {
	f, j, c := delivery(t, "idle")
	journalWithTwoConversations(t, j)

	d := Deliver(context.Background(), c, j, "ba614444", "wE:p17", "Payment закрыт: SNEW-1686",
		DeliverOptions{Stage: StageFinished, WantSession: leaderSession,
			Deadline: time.Second, Poll: 10 * time.Millisecond})
	if d.OK {
		t.Fatal("идентификатор совпал, но панель ведёт несколько разговоров — писать нельзя")
	}
	if d.Cause != CauseSharedPane {
		t.Fatalf("причина машиночитаема, получено %q", d.Cause)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("в активный разговор не ушло ничего")
	}
	if !d.Fallback {
		t.Fatal("человеку сказано")
	}
}

func TestForeignReportNeverBecomesAMessageOfAnotherConversation(t *testing.T) {
	// Запасной канал — уведомление herdr, а не реплика. Отчёт по багам не
	// должен превратиться в сообщение лицензионного разговора.
	f, j, c := delivery(t, "idle")
	f.Reply("agent.get", agentWith("idle", "01a07921"))
	j.Append(journal.Record{Task: "ba614444", Event: journal.Started, Pane: "wE:p2N",
		Target: "wE:p17", TargetSession: leaderSession})

	d := Deliver(context.Background(), c, j, "ba614444", "wE:p17", "Payment закрыт: SNEW-1686/1687",
		DeliverOptions{Stage: StageFinished, WantSession: leaderSession,
			Deadline: time.Second, Poll: 10 * time.Millisecond})
	if d.Cause != CauseWrongConversation {
		t.Fatalf("причина wrong_conversation, получено %q", d.Cause)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("ни одной реплики в чужой разговор")
	}
	body, _ := f.LastRequest("notification.show")["params"].(map[string]any)["body"].(string)
	if !strings.Contains(body, "SNEW-1686") {
		t.Fatalf("сам отчёт ушёл человеку, получено %q", body)
	}
	recs, _ := j.Read()
	last := recs[len(recs)-1]
	if last.Event != journal.Notified || last.Cause != CauseWrongConversation {
		t.Fatalf("причина попала в журнал, получено %+v", last)
	}
	if strings.HasPrefix(last.Outcome, WokeUp) {
		t.Fatalf("доставленным это не считается, получено %q", last.Outcome)
	}
}

func TestLateReportIntoASharedPaneIsAlsoRefused(t *testing.T) {
	// Поздний отчёт приходит через час, когда панель тем более успела
	// сменить разговор.
	f, j, c := delivery(t, "idle")
	journalWithTwoConversations(t, j)
	j.Append(journal.Record{Task: "ba614444", Event: journal.Finished, Outcome: TimedOut})

	res, _ := Report(context.Background(), "ba614444", "готово", "SNEW-1686 и SNEW-1687 закрыты",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p2N", Deadline: time.Second, Poll: 10 * time.Millisecond})
	if res.Delivered == nil || res.Delivered.OK {
		t.Fatalf("в общую панель поздний отчёт не пишется, получено %+v", res.Delivered)
	}
	if res.Delivered.Cause != CauseSharedPane {
		t.Fatalf("причина названа, получено %q", res.Delivered.Cause)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("ни одной реплики в чужой разговор")
	}
}

func TestLostCarriesTheMachineReadableCause(t *testing.T) {
	now := time.Now()
	got := LostReports([]journal.Record{
		{Task: "ba614444", Time: now, Event: journal.Reported, Outcome: "готово", Reason: "SNEW-1686"},
		{Task: "ba614444", Time: now, Event: journal.Notified, Stage: StageFinished,
			Target: "wE:p17", Cause: CauseWrongConversation, Outcome: NotDelivered},
	})
	if len(got) != 1 || got[0].Cause != CauseWrongConversation {
		t.Fatalf("причина доезжает до недоставленного, получено %+v", got)
	}
}

func TestStateOfTaskIsAddressingWithoutPanes(t *testing.T) {
	// Идентификатор поручения держит только тот разговор, который его затеял:
	// вытягивание по нему не нуждается ни в панели, ни в сессии.
	now := time.Now()
	recs := []journal.Record{
		{Task: "чужая", Time: now, Event: journal.Started, Pane: "wE:pX"},
		{Task: "моя", Time: now, Event: journal.Started, Pane: "wE:p2N", Target: "wE:p17"},
		{Task: "моя", Time: now, Event: journal.Reported, Outcome: "готово", Reason: "SNEW-1686"},
		{Task: "моя", Time: now, Event: journal.Finished, Outcome: "отчиталась"},
		{Task: "моя", Time: now, Event: journal.Notified, Stage: StageFinished,
			Outcome: NotDelivered, Cause: CauseSharedPane},
	}
	st := StateOfTask(recs, "моя")
	if !st.Known || st.Pane != "wE:p2N" || st.Target != "wE:p17" {
		t.Fatalf("получено %+v", st)
	}
	if !strings.Contains(st.Report, "SNEW-1686") || st.Outcome != "отчиталась" {
		t.Fatalf("отчёт и исход на месте, получено %+v", st)
	}
	if len(st.Deliveries) != 1 || st.Deliveries[0].Cause != CauseSharedPane {
		t.Fatalf("судьба доставки видна, получено %+v", st.Deliveries)
	}
	if strings.Contains(st.Report, "чужая") {
		t.Fatal("чужого в выдаче нет")
	}
	if StateOfTask(recs, "нетакая").Known {
		t.Fatal("незнакомое поручение помечено неизвестным")
	}
}
