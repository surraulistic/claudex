package supervise

import (
	"strings"

	"context"
	"github.com/surraulistic/claudex/internal/codex"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	var seen []task.FlushOptions
	s := New(Options{Journal: j, StatusPath: path, Flush: spyFlush(true, &seen)})
	s.Once(context.Background())

	st, alive := Read(path)
	if !alive {
		t.Fatal("наш же процесс считается живым")
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

func TestSupervisorWakesForSilentWorkers(t *testing.T) {
	// Главное, ради чего затевался хук: задача не вызвала done, а затеявший
	// всё равно узнаёт.
	sent := codexHomeFor(t, "01a0a268-ca14-7441-844b-8fcd24cd2e45")
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
	sent := codexHomeFor(t, "01a0a268-ca14-7441-844b-8fcd24cd2e45")
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
