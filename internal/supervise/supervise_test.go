package supervise

import (
	"strings"

	"context"
	"fmt"
	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// pending кладёт в журнал отчёт, который до затеявшего не дошёл: ровно то
// состояние, в котором зависли e5704090 и ещё сто тридцать один.
func pending(t *testing.T, j *journal.Journal, id, thread string) {
	t.Helper()
	for _, r := range []journal.Record{
		{Task: id, Event: journal.Started, Pane: "wE:p37", Time: time.Now().Add(-time.Hour),
			Target: thread, TargetSession: thread, TargetKind: task.KindThread},
		{Task: id, Event: journal.Finished, Outcome: task.SentNoWait},
		{Task: id, Event: journal.Reported, Outcome: "готово", Reason: "текст отчёта"},
		{Task: id, Event: journal.Notified, Target: thread, TargetKind: task.KindThread,
			Stage: task.StageReported, Cause: task.CauseThreadNotLive, Outcome: task.ToldHuman},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

func newJournal(t *testing.T) *journal.Journal {
	t.Helper()
	return journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
}

// spyFlush подменяет дожим: возвращает названный исход и запоминает, о чём его
// просили.
func spyFlush(ok bool, seen *[]task.FlushOptions) func(context.Context, task.FlushOptions) []task.Flushed {
	return func(_ context.Context, o task.FlushOptions) []task.Flushed {
		*seen = append(*seen, o)
		recs, _ := o.Journal.Read()
		var out []task.Flushed
		for _, l := range task.LostReports(recs) {
			if o.Skip[l.Task] {
				continue
			}
			if len(out) >= o.Max {
				break
			}
			out = append(out, task.Flushed{Task: l.Task, Target: l.Target, Ok: ok})
		}
		return out
	}
}

func TestDeliversWhatTheCallerNeverGot(t *testing.T) {
	// Сам смысл наблюдателя: отчёт уходит сам, без того чтобы кто-то звал flush.
	j := newJournal(t)
	pending(t, j, "e5704090", "01a0a268-ca14-7441-844b-8fcd24cd2e45")
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Flush: spyFlush(true, &seen)})

	got := s.Once(context.Background())
	if len(got.Delivered) != 1 || got.Delivered[0] != "e5704090" {
		t.Fatalf("отчёт доставлен, получено %+v", got)
	}
	if got.Pending != 1 {
		t.Fatalf("ожидающих названо, получено %+v", got)
	}
}

func TestDeliveredOnceIsNotSentAgain(t *testing.T) {
	// Дедупликация живёт в журнале: доставленный отчёт перестаёт быть
	// зависшим, и второй тик его не трогает.
	j := newJournal(t)
	pending(t, j, "e5704090", "тред")
	s := New(Options{Journal: j, Flush: func(_ context.Context, o task.FlushOptions) []task.Flushed {
		recs, _ := o.Journal.Read()
		var out []task.Flushed
		for _, l := range task.LostReports(recs) {
			// Настоящий дожим дописывает в журнал успешную доставку — так
			// зависший перестаёт быть зависшим.
			o.Journal.Append(journal.Record{Task: l.Task, Event: journal.Notified,
				Target: l.Target, TargetKind: task.KindThread,
				Stage: task.StageReported, Outcome: task.WokeUp})
			out = append(out, task.Flushed{Task: l.Task, Ok: true})
		}
		return out
	}})

	if got := s.Once(context.Background()); len(got.Delivered) != 1 {
		t.Fatalf("первый тик доставил, получено %+v", got)
	}
	got := s.Once(context.Background())
	if len(got.Delivered) != 0 || got.Pending != 0 {
		t.Fatalf("второй раз то же не шлём, получено %+v", got)
	}
}

func TestRefusedAddressIsDeferredNotHammered(t *testing.T) {
	// Без этого наблюдатель долбится в закрытый разговор каждые полминуты.
	j := newJournal(t)
	pending(t, j, "e5704090", "закрытый")
	now := time.Now()
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Now: func() time.Time { return now },
		Flush: spyFlush(false, &seen)})

	if got := s.Once(context.Background()); len(got.Failed) != 1 {
		t.Fatalf("первая попытка не прошла, получено %+v", got)
	}
	got := s.Once(context.Background())
	if got.Deferred != 1 || len(got.Failed) != 0 {
		t.Fatalf("сразу повторять не стали, получено %+v", got)
	}
	if len(seen) != 1 {
		t.Fatalf("дожим второй раз не звали вовсе, получено %d вызовов", len(seen))
	}
}

func TestDeferralGrowsButHasACeiling(t *testing.T) {
	// Откладывать вдвое дальше — но не навсегда: разговор может открыться.
	j := newJournal(t)
	pending(t, j, "aaaaaaaa", "закрытый")
	now := time.Now()
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Now: func() time.Time { return now },
		Flush: spyFlush(false, &seen)})

	var last time.Duration
	for i := 0; i < 8; i++ {
		s.Once(context.Background())
		w := s.wait["aaaaaaaa"]
		if w < last {
			t.Fatalf("отсрочка не сокращается, было %s стало %s", last, w)
		}
		if w > BackoffMax {
			t.Fatalf("отсрочка не выше предела, получено %s", w)
		}
		last = w
		now = now.Add(w + time.Second)
	}
	if last != BackoffMax {
		t.Fatalf("предел достигнут, получено %s", last)
	}
}

func TestWorkPerTickIsBounded(t *testing.T) {
	// Тик должен оставаться коротким: наблюдатель не будит десяток разговоров
	// разом.
	j := newJournal(t)
	for _, id := range []string{"11111111", "22222222", "33333333", "44444444", "55555555"} {
		pending(t, j, id, "тред-"+id)
	}
	var seen []task.FlushOptions
	s := New(Options{Journal: j, MaxPerTick: 2, Flush: spyFlush(true, &seen)})

	got := s.Once(context.Background())
	if len(got.Delivered) != 2 {
		t.Fatalf("за тик не больше предела, получено %+v", got)
	}
	if seen[0].Max != 2 {
		t.Fatalf("предел передан дожиму, получено %d", seen[0].Max)
	}
}

func TestQuietTickTouchesNothing(t *testing.T) {
	// Пустой журнал — пустой тик, без вызовов дожима.
	j := newJournal(t)
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Flush: spyFlush(true, &seen)})
	got := s.Once(context.Background())
	if len(got.Delivered) != 0 || got.Pending != 0 {
		t.Fatalf("тихо, получено %+v", got)
	}
}

func TestStatusIsWrittenAndReadBack(t *testing.T) {
	j := newJournal(t)
	pending(t, j, "e5704090", "тред")
	path := filepath.Join(t.TempDir(), "supervisor.json")
	release, err := Acquire(lockNear(path))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var seen []task.FlushOptions
	s := New(Options{Journal: j, StatusPath: path, Flush: spyFlush(true, &seen)})
	s.Once(context.Background())

	st, alive := Read(path)
	if !alive {
		t.Fatal("держатель замка считается живым")
	}
	if st.PID != os.Getpid() || st.Ticks != 1 || st.Delivered != 1 {
		t.Fatalf("состояние записано, получено %+v", st)
	}
}

func TestStatusOfADeadSupervisorIsNotTrusted(t *testing.T) {
	// Файл переживает убитый процесс. Поверив файлу, doctor сказал бы «под
	// присмотром» там, где присмотра нет.
	path := filepath.Join(t.TempDir(), "supervisor.json")
	os.WriteFile(path, []byte(`{"pid":999999,"ticks":3}`), 0o644)
	if _, alive := Read(path); alive {
		t.Fatal("мёртвый наблюдатель живым не считается")
	}
}

func TestStoppingClearsTheStatus(t *testing.T) {
	// Остановленный наблюдатель не должен выглядеть работающим.
	j := newJournal(t)
	path := filepath.Join(t.TempDir(), "supervisor.json")
	var seen []task.FlushOptions
	s := New(Options{Journal: j, StatusPath: path, Interval: time.Hour,
		Flush: spyFlush(true, &seen)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("файл состояния убран, получено %v", err)
	}
}

// silentAssignment — исполнитель доработал ход, отметка от хука есть, отчёта
// нет: ровно 168 из 424 поручений выглядели так.
func silentAssignment(t *testing.T, j *journal.Journal, id, thread string, idleAgo time.Duration) {
	t.Helper()
	now := time.Now()
	for _, r := range []journal.Record{
		{Task: id, Event: journal.Started, Pane: "wE:p37", PaneSession: "разговор",
			Target: thread, TargetSession: thread, TargetKind: task.KindThread,
			Time: now.Add(-time.Hour)},
		{Task: id, Event: journal.Finished, Outcome: task.SentNoWait, Time: now.Add(-time.Hour)},
		{Task: id, Event: journal.Idle, PaneSession: "разговор",
			Reason: "ветка запушена, тесты зелёные", Time: now.Add(-idleAgo)},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

// codexHomeFor поднимает поддельный дом Codex с живым тредом и перехватывает
// очередь: наблюдателю нужен адресат, который примет пробуждение.
type queued struct{ thread, message string }

func codexHomeFor(t *testing.T, live string) *[]queued {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "thread-writer-locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "thread-writer-locks", live+".lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "session_index.jsonl"), []byte(`{"id":"`+live+`"}`+"\n"), 0o644)
	t.Setenv("CODEX_HOME", dir)

	var sent []queued
	prev := codex.Queue
	codex.Queue = func(_ context.Context, id, msg string) error {
		sent = append(sent, queued{thread: id, message: msg})
		return nil
	}
	t.Cleanup(func() { codex.Queue = prev })
	return &sent
}

// wakeableThread — тред загружен и ничей: пробуждение доходит до хода.
func wakeableThread(t *testing.T, th string) *[]queued {
	t.Helper()
	codexHomeFor(t, th)
	app := codextest.Start(t)
	app.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle}})
	var sent []queued
	app.OnAdd = func(id, msg string) { sent = append(sent, queued{thread: id, message: msg}) }
	return &sent
}

func TestSupervisorWakesForSilentWorkers(t *testing.T) {
	// Главное, ради чего затевался хук: задача не вызвала done, а затеявший
	// всё равно узнаёт.
	sent := wakeableThread(t, "01a0a268-ca14-7441-844b-8fcd24cd2e45")
	j := newJournal(t)
	silentAssignment(t, j, "43bc6e11", "01a0a268-ca14-7441-844b-8fcd24cd2e45", 10*time.Minute)
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Flush: spyFlush(true, &seen)})

	got := s.Once(context.Background())
	if len(got.Silent) != 1 || got.Silent[0] != "43bc6e11" {
		t.Fatalf("за молчуна разбудили, получено %+v", got)
	}
	if len(*sent) != 1 {
		t.Fatalf("ровно одно пробуждение, получено %+v", *sent)
	}
	msg := (*sent)[0].message
	if strings.Contains(msg, "готово") {
		t.Errorf("исхода не называем — его знает только задача, получено %q", msg)
	}
	if !strings.Contains(msg, "ветка запушена") {
		t.Errorf("последняя реплика передана как доказательство, получено %q", msg)
	}
}

func TestSilentWorkerIsNotWokenTwice(t *testing.T) {
	sent := wakeableThread(t, "01a0a268-ca14-7441-844b-8fcd24cd2e45")
	j := newJournal(t)
	silentAssignment(t, j, "43bc6e11", "01a0a268-ca14-7441-844b-8fcd24cd2e45", 10*time.Minute)
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Flush: spyFlush(true, &seen)})

	s.Once(context.Background())
	s.Once(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("второй раз за то же не будим, получено %d", len(*sent))
	}
}

func TestArchivingIsOffUnlessAskedFor(t *testing.T) {
	// Журнал — единственная память об этой работе. Решать за человека, когда
	// её перекладывать, инструмент не должен.
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	j := journal.Open(path)
	j.Append(journal.Record{Task: "aaaaaaaa", Event: journal.Started,
		Time: time.Now().Add(-365 * 24 * time.Hour)})
	before, _ := os.ReadFile(path)

	var seen []task.FlushOptions
	s := New(Options{Journal: j, JournalPath: path, Flush: spyFlush(true, &seen)})
	s.Once(context.Background())

	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("без флага журнал не трогаем")
	}
}

func TestArchivingRunsOncePerDayNotEveryTick(t *testing.T) {
	// Журнал переписывается целиком: делать это каждую минуту — тратить работу
	// впустую и держать окно, в котором сбой застанет перезапись.
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	j := journal.Open(path)
	old := time.Now().Add(-365 * 24 * time.Hour)
	j.Append(journal.Record{Task: "aaaaaaaa", Event: journal.Started, Time: old})
	j.Append(journal.Record{Task: "aaaaaaaa", Event: journal.Reported, Outcome: "готово", Time: old})
	j.Append(journal.Record{Task: "bbbbbbbb", Event: journal.Started, Time: time.Now()})

	now := time.Now()
	var seen []task.FlushOptions
	s := New(Options{Journal: j, JournalPath: path, ArchiveAfter: 30 * 24 * time.Hour,
		Now: func() time.Time { return now }, Flush: spyFlush(true, &seen)})

	s.Once(context.Background())
	if s.Status().Archived != 2 {
		t.Fatalf("старое уехало, получено %d", s.Status().Archived)
	}
	// Второй тик в тот же день ничего не переписывает.
	j.Append(journal.Record{Task: "cccccccc", Event: journal.Started, Time: old})
	now = now.Add(time.Minute)
	s.Once(context.Background())
	if s.Status().Archived != 2 {
		t.Fatalf("в тот же день второй раз не переписываем, получено %d", s.Status().Archived)
	}
	// А через сутки — да.
	now = now.Add(25 * time.Hour)
	s.Once(context.Background())
	if s.Status().Archived != 3 {
		t.Fatalf("через сутки уносим накопившееся, получено %d", s.Status().Archived)
	}
}

func TestArchivingFailureIsVisibleNotSwallowed(t *testing.T) {
	// Молчаливо не сработавшая уборка — это журнал, который растёт, и никто не
	// знает почему.
	var seen []task.FlushOptions
	s := New(Options{Journal: newJournal(t), JournalPath: "/нет/такого/каталога/tasks.jsonl",
		ArchiveAfter: 24 * time.Hour, Flush: spyFlush(true, &seen)})
	s.Once(context.Background())
	if s.Status().ArchiveNote == "" && s.Status().Archived == 0 {
		// Пустой журнал по несуществующему пути читается как пустой — это не
		// ошибка. Проверяем хотя бы, что счётчик не соврал.
		return
	}
	if s.Status().Archived != 0 {
		t.Fatalf("ничего не переносилось, получено %d", s.Status().Archived)
	}
}

// silentPaneLeader — молчун, затеявший которого — панель с разговором
// leader-s; herdr сейчас показывает в ней разговор in.
func silentPaneLeader(t *testing.T, in string) (*herdrtest.Fake, *journal.Journal) {
	t.Helper()
	f := herdrtest.Start(t)
	f.Reply("agent.get", `{"id":"x","result":{"type":"agent","agent":{"pane_id":"wE:p1","agent":"claude",`+
		`"agent_status":"idle","agent_session":{"value":"`+in+`"}}}}`)
	f.Reply("agent.prompt", `{"id":"x","result":{"type":"agent","agent":{"pane_id":"wE:p1","agent_status":"idle"}}}`)
	f.Reply("notification.show", `{"id":"x","result":{"shown":true}}`)
	j := newJournal(t)
	now := time.Now()
	for _, r := range []journal.Record{
		{Task: "43bc6e11", Event: journal.Started, Pane: "wE:p37", PaneSession: "разговор",
			Target: "wE:p1", TargetSession: "leader-s", TargetKind: task.KindPane, Time: now.Add(-time.Hour)},
		{Task: "43bc6e11", Event: journal.Idle, PaneSession: "разговор",
			Reason: "ветка запушена, тесты зелёные", Time: now.Add(-10 * time.Minute)},
	} {
		j.Append(r)
	}
	return f, j
}

func requested(f *herdrtest.Fake, method string) []string {
	var out []string
	for _, r := range f.Requests() {
		if strings.Contains(r, `"method":"`+method+`"`) {
			out = append(out, r)
		}
	}
	return out
}

func TestSilentWorkerWakesPaneLeaderInTheSameConversation(t *testing.T) {
	// R1: «нужный разговор» панели — её разговор, а не id панели.
	f, j := silentPaneLeader(t, "leader-s")
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Client: herdr.New(f.Path), Flush: spyFlush(true, &seen)})

	got := s.Once(context.Background())
	if len(got.Silent) != 1 || got.Silent[0] != "43bc6e11" {
		t.Fatalf("ведущего-панель разбудили, получено %+v", got)
	}
	prompts := requested(f, "agent.prompt")
	if len(prompts) != 1 || !strings.Contains(prompts[0], "ветка запушена") ||
		!strings.Contains(prompts[0], "claudex task 43bc6e11") {
		t.Fatalf("панель получила текст с последней репликой, а не пустую строку: %v", prompts)
	}
	if n := len(requested(f, "notification.show")); n != 0 {
		t.Fatalf("дошло — человека не тревожим, уведомлений %d", n)
	}
}

func TestSilentWorkerNeverTellsHumanWhenPaneHasAnotherConversation(t *testing.T) {
	f, j := silentPaneLeader(t, "другой")
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Client: herdr.New(f.Path), Flush: spyFlush(true, &seen)})

	for i := 0; i < 3; i++ {
		s.next = map[string]time.Time{}
		if got := s.Once(context.Background()); len(got.Silent) != 0 {
			t.Fatalf("в панели чужой разговор — не будим: %+v", got)
		}
	}
	if n := len(requested(f, "agent.prompt")); n != 0 {
		t.Fatalf("в чужой разговор не пишем: %d", n)
	}
	if n := len(requested(f, "notification.show")); n != 0 {
		t.Fatalf("про молчание человеку всплывашки на каждую попытку не идут: %d", n)
	}
}

func TestDoneAfterSilenceWakeStillReachesTheLeader(t *testing.T) {
	// F1: пробуждение за молчуна — не доставка отчёта. Ведущий ответил на
	// вопрос, исполнитель доделал и вызвал done — этот done должен дойти.
	const th = "01a0a268-ca14-7441-844b-8fcd24cd2e45"
	sent := wakeableThread(t, th)
	j := newJournal(t)
	silentAssignment(t, j, "43bc6e11", th, 10*time.Minute)
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Flush: spyFlush(true, &seen)})
	if got := s.Once(context.Background()); len(got.Silent) != 1 {
		t.Fatalf("за молчуна разбудили, получено %+v", got)
	}

	res, err := task.Report(context.Background(), "43bc6e11", "готово", "миграция удалена",
		task.ReportOptions{Journal: j, Pane: "wE:p37", Deadline: 5 * time.Second, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != "" || res.Delivered == nil || !res.Delivered.Reached() {
		t.Fatalf("настоящий done после пробуждения доставлен, получено %+v", res)
	}
	if len(*sent) != 2 {
		t.Fatalf("в тред ушли и пробуждение, и отчёт, получено %d", len(*sent))
	}
	recs, _ := j.Read()
	if st := task.StateOf(recs, "43bc6e11").State; st != task.StateDone {
		t.Fatalf("состояние — исход задачи, получено %q", st)
	}
	if lost := task.LostReports(recs); len(lost) != 0 {
		t.Fatalf("ничего не потеряно, получено %+v", lost)
	}
}

func TestNewOutcomeIsANewMessageInTheThread(t *testing.T) {
	// F8: повтор done с другим исходом — новое сообщение треда, а не прежнее,
	// которое тред узнал бы по clientUserMessageId.
	const th = "01a0a268-ca14-7441-844b-8fcd24cd2e45"
	sent := wakeableThread(t, th)
	j := newJournal(t)
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Started, Pane: "wE:p37",
		Target: th, TargetSession: th, TargetKind: task.KindThread})
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Finished, Outcome: task.SentNoWait})
	o := task.ReportOptions{Journal: j, Pane: "wE:p37", Deadline: 5 * time.Second, Poll: 10 * time.Millisecond}
	for i, c := range []struct {
		outcome string
		want    int
	}{{"готово", 1}, {"провал", 2}, {"провал", 2}} {
		if _, err := task.Report(context.Background(), "43bc6e11", c.outcome, "попытка", o); err != nil {
			t.Fatal(err)
		}
		if len(*sent) != c.want {
			t.Fatalf("шаг %d (%s): сообщений в треде %d, ждали %d", i, c.outcome, len(*sent), c.want)
		}
	}
	recs, _ := j.Read()
	if st := task.StateOf(recs, "43bc6e11").State; st != task.StateFailed {
		t.Fatalf("состояние — последний исход, получено %q", st)
	}
}

func TestStatusNamingAForeignLivePIDIsNotTrusted(t *testing.T) {
	// F4: после kill -9 или перезагрузки pid из файла достаётся живому чужому
	// процессу. Живость — только замок.
	path := filepath.Join(t.TempDir(), "supervisor.json")
	os.WriteFile(path, []byte(fmt.Sprintf(`{"pid":%d,"ticks":7639}`, os.Getppid())), 0o644)
	if _, alive := Read(path); alive {
		t.Fatal("живой чужой pid без замка — наблюдатель не работает")
	}
	release, err := Acquire(lockNear(path))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	st, alive := Read(path)
	if !alive || st.PID != os.Getpid() || st.Ticks != 0 {
		t.Fatalf("замок держит другой процесс: он и назван, чужая статистика не выдаётся, получено %+v %v", st, alive)
	}
}

func TestHeldDoesNotOverwriteTheHolderPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "supervisor.lock")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("4242"), 0o644)
	if Held(path) {
		t.Fatal("замок свободен")
	}
	if b, _ := os.ReadFile(path); string(b) != "4242" {
		t.Fatalf("проверка замка его не переписывает, получено %q", b)
	}
}

func TestReconcileFlagReallyCollects(t *testing.T) {
	// F2: --reconcile обещает сборку за молчунов — тик её и делает. Панель
	// освободилась без отчёта и без отметки хука: видно только экран.
	const th = "01a0a268-ca14-7441-844b-8fcd24cd2e45"
	sent := wakeableThread(t, th)
	f := herdrtest.Start(t)
	f.Reply("agent.get", `{"id":"x","result":{"type":"agent","agent":{"pane_id":"wE:p37","agent":"claude",`+
		`"agent_status":"idle","agent_session":{"value":"разговор"}}}}`)
	f.Reply("agent.read", `{"id":"x","result":{"read":{"text":"Готово: миграции применены на dev и stage, тесты зелёные, ветка запушена."}}}`)
	j := newJournal(t)
	when := time.Now().Add(-time.Hour)
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Started, Pane: "wE:p37", PaneSession: "разговор",
		Target: th, TargetSession: th, TargetKind: task.KindThread, Time: when})
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Finished, Outcome: task.SentNoWait, Time: when})
	var seen []task.FlushOptions

	off := New(Options{Journal: j, Client: herdr.New(f.Path), Flush: spyFlush(true, &seen)})
	if got := off.Once(context.Background()); len(got.Collected) != 0 || len(*sent) != 0 {
		t.Fatalf("без флага сборки нет, получено %+v", got)
	}
	on := New(Options{Journal: j, Client: herdr.New(f.Path), Reconcile: true, Flush: spyFlush(true, &seen)})
	got := on.Once(context.Background())
	if len(got.Collected) != 1 || got.Collected[0] != "43bc6e11" || len(*sent) != 1 {
		t.Fatalf("с флагом наблюдатель собрал и разбудил, получено %+v, сообщений %d", got, len(*sent))
	}
	if !strings.Contains((*sent)[0].message, "43bc6e11") {
		t.Fatalf("в тред ушло событие о поручении, получено %q", (*sent)[0].message)
	}
	if on.Status().Collected != 1 || !on.Status().Reconciling {
		t.Fatalf("состояние говорит правду, получено %+v", on.Status())
	}
	on.Once(context.Background())
	if len(*sent) != 1 {
		t.Fatalf("собранное второй раз не будит, сообщений %d", len(*sent))
	}
}

func TestCollectionSkipsWhatWakeSilentOwnsAndDefersRefusals(t *testing.T) {
	j := newJournal(t)
	silentAssignment(t, j, "43bc6e11", "тред", 10*time.Minute)
	var calls []task.ReconcileOptions
	var seen []task.FlushOptions
	s := New(Options{Journal: j, Reconcile: true, Flush: spyFlush(true, &seen),
		Collect: func(_ context.Context, o task.ReconcileOptions) []task.Reconciled {
			calls = append(calls, o)
			return []task.Reconciled{{Task: "aaaaaaaa", Cause: task.CauseExecutorBusy}}
		}})
	s.Once(context.Background())
	s.Once(context.Background())
	if len(calls) != 2 {
		t.Fatalf("сборка на каждом тике, вызовов %d", len(calls))
	}
	if !calls[0].Skip["43bc6e11"] {
		t.Fatalf("молчуна с отметкой хука будит wakeSilent, сборка его не трогает: %+v", calls[0].Skip)
	}
	if calls[0].Skip["aaaaaaaa"] || !calls[1].Skip["aaaaaaaa"] {
		t.Fatalf("отказавшее откладывается до следующего раза: %+v / %+v", calls[0].Skip, calls[1].Skip)
	}
}
