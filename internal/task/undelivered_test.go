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

func TestClosedThreadKeepsTheReportInThePullChannel(t *testing.T) {
	// Закрытый тред — это не доставка. Показ человеку ею тоже не является:
	// человек прочитает всплывашку, когда будет за машиной, а разговор так и не
	// узнает, что поручение кончилось.
	codexHome(t) // ни одного живого замка; otherThread известен, но закрыт
	f, j, c := threadDelivery(t)
	seedThreadTask(t, j, "465a77fc", otherThread)

	res, err := Report(context.Background(), "465a77fc", "готово", reportText,
		ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
			Deadline: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered == nil || res.Delivered.OK {
		t.Fatalf("в закрытый тред не доставляется, получено %+v", res.Delivered)
	}
	if res.Delivered.Cause != CauseThreadNotLive {
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
	if found.Cause != CauseThreadNotLive || found.Target != otherThread {
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

func TestReopenedThreadGetsTheReportOnTheNextDone(t *testing.T) {
	// Закрытый разговор открывается снова — и следующий done обязан попасть.
	// Прежде повтор подавлялся: показ человеку помечал стадию доставленной.
	codexHome(t)
	_, j, c := threadDelivery(t)
	seedThreadTask(t, j, "465a77fc", otherThread)

	o := ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
		Deadline: time.Second, Poll: time.Millisecond}
	if _, err := Report(context.Background(), "465a77fc", "готово", reportText, o); err != nil {
		t.Fatal(err)
	}

	sent := codexHome(t, otherThread) // разговор вернулся
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
	seedThreadTask(t, j, "465a77fc", otherThread)
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

	res, err := Report(context.Background(), "465a77fc", "готово", reportText,
		ReportOptions{Client: c, Journal: j, Pane: "wE:p37",
			Deadline: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered.OK {
		t.Fatal("живой сосед адресом не становится")
	}
	if len(*sent) != 0 {
		t.Fatalf("в чужой разговор не писали, получено %+v", *sent)
	}
}
