package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

func TestEventCarriesWhatHappenedAndWhereToRead(t *testing.T) {
	e := Event{Task: "43bc6e11", State: StateDone, Executor: "wE:p13",
		Reason: "миграции применены"}
	got := e.Text()
	for _, want := range []string{"43bc6e11", "готово", "wE:p13", "миграции применены",
		"claudex task 43bc6e11", "claudex task log 43bc6e11"} {
		if !strings.Contains(got, want) {
			t.Errorf("в событии есть %q, получено:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Это событие, а не отчёт") {
		t.Error("событие называет себя событием, иначе его примут за ответ")
	}
}

func TestEventNeverCarriesTheReportItself(t *testing.T) {
	// Обрезанный отчёт хуже отсутствующего: он выглядит полным. Поэтому
	// причина — одна короткая строка, а не начало текста.
	long := strings.Repeat("подробности работы ", 60)
	e := Event{Task: "43bc6e11", State: StateDone, Reason: long}
	got := e.Text()
	if len([]rune(got)) > 400 {
		t.Fatalf("событие остаётся событием, получено %d символов", len([]rune(got)))
	}
	if !strings.Contains(got, "…") {
		t.Error("обрезка видна, а не выдаётся за целое")
	}
}

func TestOnlyThoseWhoCanReadGetAnEvent(t *testing.T) {
	// «Иди прочитай» выполнимо для того, кто запускает команды. Человеку у
	// панели herdr это значит переложить работу инструмента на него.
	for kind, want := range map[string]bool{
		KindThread: true, KindSession: true, KindPane: false, "": false,
	} {
		if got := WakeCapable(kind); got != want {
			t.Errorf("%q: способность читать самому = %v, получено %v", kind, want, got)
		}
	}
}

func TestPaneCallerStillGetsTheWholeReport(t *testing.T) {
	// Не недоделка, а разные адресаты: у панели читает человек.
	e := &Event{Task: "43bc6e11", State: StateDone}
	got, mode := payload("полный отчёт", DeliverOptions{Event: e}, KindPane)
	if mode != DeliveryFull || got != "полный отчёт" {
		t.Fatalf("панели — отчёт целиком, получено %q / %s", got, mode)
	}
}

func TestFullReportCanBeDemandedExplicitly(t *testing.T) {
	// Совместимость: у прежних вызывающих отчёт должен остаться отчётом.
	e := &Event{Task: "43bc6e11", State: StateDone}
	got, mode := payload("полный отчёт", DeliverOptions{Event: e, FullReport: true}, KindThread)
	if mode != DeliveryFull || got != "полный отчёт" {
		t.Fatalf("по требованию — целиком, получено %q / %s", got, mode)
	}
}

func TestComposeIsSkippedInEventMode(t *testing.T) {
	// Дайджест собирается недёшево, и собирать его ради события незачем.
	called := false
	e := &Event{Task: "43bc6e11", State: StateDone}
	got, mode := payload("сводка", DeliverOptions{Event: e,
		Compose: func(s string) string { called = true; return s + " + дайджест" }}, KindThread)
	if mode != DeliveryEvent {
		t.Fatalf("тред будится событием, получено %s", mode)
	}
	if called {
		t.Error("дайджест для события не собирается")
	}
	if strings.Contains(got, "дайджест") {
		t.Errorf("в событии дайджеста нет, получено %q", got)
	}
}

func TestDeliveryModeIsRecordedInTheJournal(t *testing.T) {
	// Иначе по записи не отличить «разбудили событием» от «прислали целиком».
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)
	d := Deliver(context.Background(), c, j, "43bc6e11", leaderThread, "сводка",
		DeliverOptions{Stage: StageReported, Kind: KindThread, WantSession: leaderThread,
			Event: &Event{Task: "43bc6e11", State: StateDone, Reason: "сделано"}})
	if !d.OK || d.Mode != DeliveryEvent {
		t.Fatalf("доставлено событием, получено %+v", d)
	}
	if len(*sent) != 1 || strings.Contains((*sent)[0].message, "сводка") {
		t.Fatalf("в разговор ушло событие, получено %+v", *sent)
	}
	recs, _ := j.Read()
	var mode string
	for _, r := range recs {
		if r.Event == journal.Notified {
			mode = r.Delivery
		}
	}
	if mode != DeliveryEvent {
		t.Fatalf("способ записан в журнал, получено %q", mode)
	}
}

func TestFailedWakeLeavesTheTaskVisible(t *testing.T) {
	// Главный запрет: событие не должно прятать потерю. Не разбудили — значит
	// поручение по-прежнему видно как недоставленное, с причиной.
	codexHome(t) // ни одного живого разговора
	_, j, c := threadDelivery(t)
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Started, Pane: "wE:p13",
		Target: otherThread, TargetSession: otherThread, TargetKind: KindThread,
		Time: time.Now()})
	j.Append(journal.Record{Task: "43bc6e11", Event: journal.Reported, Outcome: "готово",
		Reason: "текст отчёта"})

	d := Deliver(context.Background(), c, j, "43bc6e11", otherThread, "сводка",
		DeliverOptions{Stage: StageReported, Kind: KindThread, WantSession: otherThread,
			HumanTold: true,
			Event:     &Event{Task: "43bc6e11", State: StateDone, Reason: "сделано"}})
	if d.OK {
		t.Fatal("закрытый разговор не разбудить")
	}
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatalf("поручение осталось видно как недоставленное, получено %+v", recs)
	}
	if got := StateOf(recs, "43bc6e11").State; got != StateUndelivered {
		t.Fatalf("состояние называет потерю, получено %s", got)
	}
}
