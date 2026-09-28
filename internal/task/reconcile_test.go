package task

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
	"github.com/surraulistic/claudex/internal/journal"
)

// Случай 260031f8: поручение ушло в панель wE:p13 в 02:40:47, задача довела
// работу до конца и написала финал, но `claudex done` не вызвала. В журнале
// остались started и finished, отчёта нет, управляющий разговор не узнал ничего.

const (
	silentTask  = "260031f8"
	silentPane  = "wE:p13"
	execSession = "7e403273-2034-4296-a833-d176bd30e03d"
	finalText   = "Готово: конфигурация лицензий выровнена, 3 сервиса переведены, " +
		"миграции применены на dev и stage, тесты зелёные. Остался payment — ждёт ключа."
)

// silentDelegation кладёт в журнал поручение без отчёта: ровно то состояние,
// в котором 260031f8 провисела два часа.
func silentDelegation(t *testing.T, j *journal.Journal, when time.Time) {
	t.Helper()
	for _, r := range []journal.Record{
		{Task: silentTask, Event: journal.Started, Pane: silentPane, Time: when,
			Target: leaderThread, TargetSession: leaderThread, TargetKind: KindThread,
			PaneSession: execSession, TargetState: "live", TargetSeen: when},
		{Task: silentTask, Event: journal.Finished, Pane: silentPane, Time: when,
			Outcome: "отправлено без ожидания"},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

// executor поднимает поддельный herdr, в котором панель отвечает названным
// состоянием и разговором.
func executor(t *testing.T, status, session string) (*herdrtest.Fake, *journal.Journal, *herdr.Client) {
	t.Helper()
	f := herdrtest.Start(t)
	f.Reply("agent.get", agentWith(status, session))
	f.Reply("agent.prompt", agent("idle"))
	f.Reply("notification.show", `{"id":"x","result":{"shown":true}}`)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	return f, j, herdr.New(f.Path)
}

func liveScreen(text string) func(string, int) (string, error) {
	return func(string, int) (string, error) { return text, nil }
}

func reconcileOpts(j *journal.Journal, c *herdr.Client, screen string) ReconcileOptions {
	return ReconcileOptions{
		Journal: j, Client: c, Read: liveScreen(screen),
		MinAge: time.Minute, Now: time.Now(),
	}
}

func TestFinalWithoutDoneIsCollectedAndDelivered(t *testing.T) {
	// Сам дефект. Панель закончила, разговор тот же, позже ей ничего не
	// поручали — значит на экране наш финал, и он должен уехать ведущему.
	sent := codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", execSession)
	silentDelegation(t, j, time.Now().Add(-2*time.Hour))

	got := Reconcile(context.Background(), reconcileOpts(j, c, finalText))
	if len(got) != 1 {
		t.Fatalf("одно поручение, получено %+v", got)
	}
	if !got[0].Synthetic || !got[0].Delivered {
		t.Fatalf("отчёт собран и доставлен, получено %+v", got[0])
	}
	// Ведущего будят событием; собранный финал лежит в журнале и читается
	// через `claudex task log`. Класть его в диалог значит подменять ответ
	// координатора служебной сводкой.
	if len(*sent) != 1 {
		t.Fatalf("ведущего разбудили ровно раз, получено %+v", *sent)
	}
	if strings.Contains((*sent)[0].message, "payment") {
		t.Fatalf("сам финал в диалог не уезжает, получено %q", (*sent)[0].message)
	}
	if !strings.Contains((*sent)[0].message, silentTask) {
		t.Fatalf("в событии назван идентификатор, получено %q", (*sent)[0].message)
	}

	// Отчёт лёг в журнал как обычный и помечен собранным.
	recs, _ := j.Read()
	var rep journal.Record
	for _, r := range recs {
		if r.Event == journal.Reported {
			rep = r
		}
	}
	if rep.Task != silentTask || !rep.Synthetic {
		t.Fatalf("запись об отчёте помечена собранной, получено %+v", rep)
	}
	if !strings.Contains(rep.Reason, "payment") {
		t.Fatalf("текст финала сохранён, получено %q", rep.Reason)
	}
}

func TestNoFinalOnScreenIsRefusedNotInvented(t *testing.T) {
	// Пустой экран — это отсутствие доказательства, а не «задача молча
	// справилась». Выдумывать отчёт нельзя.
	sent := codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", execSession)
	silentDelegation(t, j, time.Now().Add(-2*time.Hour))

	got := Reconcile(context.Background(), reconcileOpts(j, c, "   \n  ▸ \n"))
	if len(got) != 1 || got[0].Synthetic {
		t.Fatalf("собирать нечего, получено %+v", got)
	}
	if got[0].Cause != CauseNoFinalOutput {
		t.Fatalf("причина названа, получено %q", got[0].Cause)
	}
	if len(*sent) != 0 {
		t.Fatal("в разговор ничего не уехало")
	}
	recs, _ := j.Read()
	for _, r := range recs {
		if r.Event == journal.Reported {
			t.Fatal("ложного отчёта в журнале нет")
		}
	}
}

func TestOnlyTheLiveScreenIsUsedAsTheFinal(t *testing.T) {
	// Индекс cass отстаёт на часы, и подставить вместо финала вчерашний текст
	// хуже, чем не подставить ничего. Источник здесь ровно один — живой экран,
	// и в отчёт попадает именно он.
	codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", execSession)
	silentDelegation(t, j, time.Now().Add(-2*time.Hour))

	o := reconcileOpts(j, c, finalText)
	var askedPane string
	o.Read = func(p string, n int) (string, error) { askedPane = p; return finalText, nil }

	got := Reconcile(context.Background(), o)
	if askedPane != silentPane {
		t.Fatalf("читали живой экран именно нашей панели, получено %q", askedPane)
	}
	recs, _ := j.Read()
	for _, r := range recs {
		if r.Event == journal.Reported && r.Reason != finalText {
			t.Fatalf("в отчёт попал живой экран без подмены, получено %q", r.Reason)
		}
	}
	if !got[0].Synthetic {
		t.Fatal("отчёт собран")
	}
}

func TestBusyPaneIsNotTreatedAsFinished(t *testing.T) {
	// Освобождение панели доказательством не считается — а работающая тем более.
	codexHome(t, leaderThread)
	_, j, c := executor(t, "working", execSession)
	silentDelegation(t, j, time.Now().Add(-2*time.Hour))

	got := Reconcile(context.Background(), reconcileOpts(j, c, finalText))
	if got[0].Synthetic || got[0].Cause != CauseExecutorBusy {
		t.Fatalf("работающую панель не трогаем, получено %+v", got[0])
	}
}

func TestAnotherConversationInThePaneIsNotOurFinal(t *testing.T) {
	// Панель переживает смену агента, разговор в ней — нет. Финал чужого
	// разговора выдать за свой нельзя.
	codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", "совсем-другой-разговор")
	silentDelegation(t, j, time.Now().Add(-2*time.Hour))

	got := Reconcile(context.Background(), reconcileOpts(j, c, finalText))
	if got[0].Synthetic || got[0].Cause != CauseExecutorChanged {
		t.Fatalf("чужой разговор отклонён, получено %+v", got[0])
	}
}

func TestALaterTaskInThePaneMeansTheScreenIsNotOurs(t *testing.T) {
	// Панели дали ещё одно поручение — значит экран показывает его финал.
	codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", execSession)
	when := time.Now().Add(-2 * time.Hour)
	silentDelegation(t, j, when)
	j.Append(journal.Record{Task: "beefbeef", Event: journal.Started,
		Pane: silentPane, Time: when.Add(time.Hour), PaneSession: execSession})

	got := Reconcile(context.Background(), reconcileOpts(j, c, finalText))
	var ours Reconciled
	for _, g := range got {
		if g.Task == silentTask {
			ours = g
		}
	}
	if ours.Synthetic || ours.Cause != CauseSupersededTask {
		t.Fatalf("экран занят чужим финалом, получено %+v", ours)
	}
}

func TestUnprovenExecutorBindingIsRefused(t *testing.T) {
	// Старые записи не знают разговора панели. Доказать, что закончил он,
	// нечем — значит не собираем.
	codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", execSession)
	j.Append(journal.Record{Task: silentTask, Event: journal.Started,
		Pane: silentPane, Time: time.Now().Add(-2 * time.Hour),
		Target: leaderThread, TargetKind: KindThread})

	got := Reconcile(context.Background(), reconcileOpts(j, c, finalText))
	if got[0].Synthetic || got[0].Cause != CauseExecutorUnknown {
		t.Fatalf("без доказанной привязки не собираем, получено %+v", got[0])
	}
}

func TestFreshSilenceIsGivenTimeToReportItself(t *testing.T) {
	// Задача может вызвать done через минуту после финала. Перехватывать её
	// незачем: собранный отчёт хуже настоящего.
	codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", execSession)
	silentDelegation(t, j, time.Now().Add(-10*time.Second))

	o := reconcileOpts(j, c, finalText)
	o.MinAge = 5 * time.Minute
	if got := Reconcile(context.Background(), o); len(got) != 0 {
		t.Fatalf("свежему молчанию даём время, получено %+v", got)
	}
}

func TestCollectedReportIsNotCollectedAgain(t *testing.T) {
	// Повтор не дублирует: собранный отчёт закрывает поручение так же, как
	// настоящий.
	sent := codexHome(t, leaderThread)
	_, j, c := executor(t, "idle", execSession)
	silentDelegation(t, j, time.Now().Add(-2*time.Hour))

	o := reconcileOpts(j, c, finalText)
	if got := Reconcile(context.Background(), o); len(got) != 1 || !got[0].Delivered {
		t.Fatalf("первый сбор прошёл, получено %+v", got)
	}
	if got := Reconcile(context.Background(), o); len(got) != 0 {
		t.Fatalf("второй раз собирать нечего, получено %+v", got)
	}
	if len(*sent) != 1 {
		t.Fatalf("в разговор ушло одно сообщение, получено %d", len(*sent))
	}
}

func TestCollectedReportGoesThroughTheSameAddressCheck(t *testing.T) {
	// Собранный отчёт не получает поблажек: закрытый разговор его не примет, и
	// он ложится в канал вытягивания, как обычный.
	sent := codexHome(t) // otherThread известен, но закрыт
	_, j, c := executor(t, "idle", execSession)
	when := time.Now().Add(-2 * time.Hour)
	j.Append(journal.Record{Task: silentTask, Event: journal.Started, Pane: silentPane,
		Time: when, Target: otherThread, TargetSession: otherThread,
		TargetKind: KindThread, PaneSession: execSession})
	j.Append(journal.Record{Task: silentTask, Event: journal.Finished, Pane: silentPane,
		Time: when, Outcome: "отправлено без ожидания"})

	got := Reconcile(context.Background(), reconcileOpts(j, c, finalText))
	if !got[0].Synthetic || got[0].Delivered {
		t.Fatalf("собран, но не доставлен, получено %+v", got[0])
	}
	if got[0].Cause != CauseThreadNotLive {
		t.Fatalf("причина та же, что у обычного отчёта, получено %q", got[0].Cause)
	}
	if len(*sent) != 0 {
		t.Fatal("в закрытый разговор не писали")
	}
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatal("собранный отчёт ждёт в канале вытягивания")
	}
}
