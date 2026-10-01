package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

// Случай 465a77fc: поручение заведено в 22:45, когда тред был жив; к отчёту в
// 23:29 разговор закрылся. Отчёт записан трижды и не ушёл в Codex ни разу, а в
// журнале значился как «показано человеку» — то есть выглядел доставленным.

const reportText = "готово # Единый механизм эмиссии и валидации токенов"

func seedThreadTask(t *testing.T, j *journal.Journal, id, thread string) {
	t.Helper()
	for _, r := range []journal.Record{
		{Task: id, Event: journal.Started, Pane: "wE:p37", Time: time.Now().Add(-time.Hour),
			Target: thread, TargetSession: thread, TargetKind: KindThread},
		{Task: id, Event: journal.Finished, Pane: "wE:p37", Outcome: "отправлено без ожидания"},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnknownThreadKeepsTheReportInThePullChannel(t *testing.T) {
	// Адрес, который подтвердить нечем, — это не доставка. Показ человеку ею
	// тоже не является: человек прочитает всплывашку, когда будет за машиной, а
	// разговор так и не узнает, что поручение кончилось.
	//
	// Закрытый тред сюда больше не относится: он очередь принимает и читает
	// при следующем пробуждении.
	codexHome(t) // unknownThread не значится в указателе сессий
	f, j, c := threadDelivery(t)
	seedThreadTask(t, j, "465a77fc", unknownThread)

	res, err := Report(context.Background(), "465a77fc", "готово", reportText,
		ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
			Deadline: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered == nil || res.Delivered.OK {
		t.Fatalf("в неподтверждённый адрес не доставляется, получено %+v", res.Delivered)
	}
	if res.Delivered.Cause != CauseThreadUnknown {
		t.Fatalf("причина названа, получено %q", res.Delivered.Cause)
	}
	if f.LastRequest("notification.show") == nil {
		t.Fatal("человеку показано")
	}

	recs, _ := j.Read()
	lost := LostReports(recs)
	var found *Lost
	for i := range lost {
		if lost[i].Task == "465a77fc" {
			found = &lost[i]
		}
	}
	if found == nil {
		t.Fatalf("отчёт остаётся в канале недоставленного, получено %+v", lost)
	}
	if !strings.Contains(found.Report, "Единый механизм") {
		t.Fatalf("отчёт сохранён целиком, получено %q", found.Report)
	}
	if found.Cause != CauseThreadUnknown || found.Target != unknownThread {
		t.Fatalf("причина и адрес сохранены, получено %+v", *found)
	}
}

func TestShowingTheHumanIsNotCountedAsReachingTheLeader(t *testing.T) {
	// Тот самый предикат. Раньше «показано человеку» считалось доставкой, и
	// отчёт исчезал из канала вытягивания.
	if ReachedLeader(ToldHuman) {
		t.Fatal("показ человеку — не пробуждение ведущего")
	}
	if ReachedLeader(NotDelivered) {
		t.Fatal("недоставленное — не пробуждение")
	}
	if !ReachedLeader(WokeUp) {
		t.Fatal("пробуждение — это доставка")
	}
}

func TestLiveThreadDeliversAndLeavesNothingUndelivered(t *testing.T) {
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)
	seedThreadTask(t, j, "465a77fc", leaderThread)

	res, err := Report(context.Background(), "465a77fc", "готово", reportText,
		ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
			Deadline: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered == nil || !res.Delivered.OK {
		t.Fatalf("живой тред принимает очередь, получено %+v", res.Delivered)
	}
	if len(*sent) != 1 || (*sent)[0].thread != leaderThread {
		t.Fatalf("ушло именно этому треду, получено %+v", *sent)
	}
	recs, _ := j.Read()
	if l := LostReports(recs); len(l) != 0 {
		t.Fatalf("недоставленного нет, получено %+v", l)
	}
}

func TestAddressThatBecomesReachableGetsTheReportOnTheNextDone(t *testing.T) {
	// Адрес был неподтверждаем, стал доступен — и следующий done обязан
	// попасть. Прежде повтор подавлялся: показ человеку помечал стадию
	// доставленной.
	//
	// Прежде это ставилось на закрытом треде; теперь закрытый тред принимает
	// очередь сразу, и недоставляемым адресом остаётся лишь неизвестный.
	codexHome(t)
	_, j, c := threadDelivery(t)
	seedThreadTask(t, j, "465a77fc", unknownThread)

	o := ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
		Deadline: time.Second, Poll: time.Millisecond}
	if _, err := Report(context.Background(), "465a77fc", "готово", reportText, o); err != nil {
		t.Fatal(err)
	}

	sent := codexHome(t, unknownThread) // тред стал известен и жив
	res, err := Report(context.Background(), "465a77fc", "готово", reportText, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered == nil || !res.Delivered.OK {
		t.Fatalf("повтор доставляет, получено %+v", res.Delivered)
	}
	if len(*sent) != 1 {
		t.Fatalf("отчёт ушёл один раз, получено %+v", *sent)
	}
	recs, _ := j.Read()
	if l := LostReports(recs); len(l) != 0 {
		t.Fatalf("после доставки недоставленного нет, получено %+v", l)
	}
}

func TestHumanIsNotPesteredOnEveryRetry(t *testing.T) {
	// Доставку ведущему повторяем, всплывашку человеку — нет.
	codexHome(t)
	f, j, c := threadDelivery(t)
	seedThreadTask(t, j, "465a77fc", unknownThread)
	o := ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
		Deadline: time.Second, Poll: time.Millisecond}

	if _, err := Report(context.Background(), "465a77fc", "готово", reportText, o); err != nil {
		t.Fatal(err)
	}
	if _, err := Report(context.Background(), "465a77fc", "готово", reportText, o); err != nil {
		t.Fatal(err)
	}
	shown := 0
	for _, r := range f.Requests() {
		if strings.Contains(r, "notification.show") {
			shown++
		}
	}
	if shown != 1 {
		t.Fatalf("человеку про ту же стадию показывают один раз, получено %d", shown)
	}
}

func TestAClosedThreadIsNeverSwappedForAnotherLiveOne(t *testing.T) {
	// Рядом живёт чужой разговор. Отправить отчёт туда значило бы разослать
	// его наугад: адрес и есть разговор, соседний — не замена.
	sent := codexHome(t, leaderThread) // жив сосед, а не адресат
	_, j, c := threadDelivery(t)
	seedThreadTask(t, j, "465a77fc", otherThread)

	if _, err := Report(context.Background(), "465a77fc", "готово", reportText,
		ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
			Deadline: time.Second, Poll: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	// Отчёт ушёл своему адресату — закрытому, но известному: его очередь
	// вычитается при следующем пробуждении. Живой сосед адресом не стал.
	if len(*sent) != 1 {
		t.Fatalf("ровно одна отправка, получено %+v", *sent)
	}
	if (*sent)[0].thread != otherThread {
		t.Fatalf("писали своему адресату, а не соседу, получено %q", (*sent)[0].thread)
	}
	if (*sent)[0].thread == leaderThread {
		t.Fatal("в чужой живой разговор не писали")
	}
}

func TestHopelessReportsStopConsumingAttempts(t *testing.T) {
	// Адрес, молчавший неделю, не ответит и сегодня. Наблюдатель при этом
	// пробовал снова и снова: на живом журнале 34 таких отчёта пережили
	// четыре тысячи проходов и не сдвинулись ни разу.
	now := time.Now()
	recs := []journal.Record{
		{Task: "aaaaaaaa", Event: journal.Reported, Outcome: "готово", Reason: "старое"},
		{Task: "aaaaaaaa", Event: journal.Notified, Stage: StageReported,
			Target: otherThread, Outcome: ToldHuman, Cause: CauseThreadUnknown,
			Time: now.Add(-8 * 24 * time.Hour)},
		{Task: "bbbbbbbb", Event: journal.Reported, Outcome: "готово", Reason: "свежее"},
		{Task: "bbbbbbbb", Event: journal.Notified, Stage: StageReported,
			Target: otherThread, Outcome: ToldHuman, Cause: CauseThreadUnknown,
			Time: now.Add(-time.Hour)},
	}
	all := LostReports(recs)
	if len(all) != 2 {
		t.Fatalf("видно оба: текст цел, ничего не прячем; получено %d", len(all))
	}
	live := LivePending(recs, now)
	if len(live) != 1 || live[0].Task != "bbbbbbbb" {
		t.Fatalf("дожимаем только свежее, получено %+v", live)
	}
}

func TestFreshFailureIsNeverCalledHopeless(t *testing.T) {
	// Граница не должна съедать то, что ещё может доехать.
	now := time.Now()
	l := Lost{Task: "aaaaaaaa", At: now.Add(-AbandonAfter + time.Minute).Format(time.RFC3339)}
	if l.Stale(now) {
		t.Fatal("младше срока — ещё пробуем")
	}
	old := Lost{Task: "bbbbbbbb", At: now.Add(-AbandonAfter - time.Minute).Format(time.RFC3339)}
	if !old.Stale(now) {
		t.Fatal("старше срока — прекращаем")
	}
}

func TestUnparsableTimeIsNotTreatedAsHopeless(t *testing.T) {
	// Сломанная отметка времени — не повод бросать отчёт: лучше лишняя
	// попытка, чем молча прекращённая доставка.
	if (Lost{At: "не дата"}).Stale(time.Now()) {
		t.Fatal("без времени считаем живым")
	}
}
