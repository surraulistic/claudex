package task

import (
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

func lc(recs ...journal.Record) []journal.Record { return recs }

func TestSentUntilTheTaskReportsItself(t *testing.T) {
	recs := lc(
		journal.Record{Task: "aaaaaaaa", Event: journal.Started, Pane: "wE:p13",
			Target: "01a0a268-ca14-7441-844b-8fcd24cd2e45", TargetKind: KindThread, Time: time.Now()},
		journal.Record{Task: "aaaaaaaa", Event: journal.Finished, Outcome: SentNoWait},
	)
	l := StateOf(recs, "aaaaaaaa")
	if l.State != StateSent {
		t.Fatalf("отправлено и ждём, получено %+v", l)
	}
	if l.Executor != "wE:p13" || l.CallerKind != KindThread {
		t.Fatalf("исполнитель и затеявший названы, получено %+v", l)
	}
}

func TestOutcomeWordDecidesDoneFailedOrNeedsInput(t *testing.T) {
	// Исход называет сама задача, и три разных слова значат три разных дела.
	for _, c := range []struct{ outcome, want string }{
		{"готово всё сделано", StateDone},
		{"частично половина", StateDone},
		{"провал не собралось", StateFailed},
		{"заблокировано нужен ключ", StateNeedsInput},
	} {
		recs := lc(
			journal.Record{Task: "bbbbbbbb", Event: journal.Started, Pane: "wE:p13"},
			journal.Record{Task: "bbbbbbbb", Event: journal.Reported, Outcome: c.outcome},
		)
		if got := StateOf(recs, "bbbbbbbb").State; got != c.want {
			t.Errorf("%q → %s, получено %s", c.outcome, c.want, got)
		}
	}
}

func TestReportThatNeverReachedTheCallerIsUndelivered(t *testing.T) {
	// Для затеявшего недоставленный отчёт неотличим от молчания, и состояние
	// обязано называть именно это — иначе «ну что там» отвечается «готово»,
	// когда он ничего не получил.
	recs := lc(
		journal.Record{Task: "cccccccc", Event: journal.Started, Pane: "wE:p13"},
		journal.Record{Task: "cccccccc", Event: journal.Reported, Outcome: "готово"},
		journal.Record{Task: "cccccccc", Event: journal.Notified, Stage: StageReported,
			Outcome: ToldHuman, Cause: CauseThreadNotLive},
	)
	if got := StateOf(recs, "cccccccc").State; got != StateUndelivered {
		t.Fatalf("недоставленный отчёт назван своим именем, получено %s", got)
	}
}

func TestDeliveredReportIsDoneNotUndelivered(t *testing.T) {
	recs := lc(
		journal.Record{Task: "dddddddd", Event: journal.Started, Pane: "wE:p13"},
		journal.Record{Task: "dddddddd", Event: journal.Reported, Outcome: "готово"},
		journal.Record{Task: "dddddddd", Event: journal.Notified, Stage: StageReported, Outcome: WokeUp},
	)
	if got := StateOf(recs, "dddddddd").State; got != StateDone {
		t.Fatalf("доставленный отчёт — готово, получено %s", got)
	}
}

func TestRefusedSendStaysCreated(t *testing.T) {
	// Исполнитель не принял задание: текст цел, но отправки не было.
	recs := lc(journal.Record{Task: "eeeeeeee", Event: journal.Refused,
		Pane: "wE:p13", Reason: "панель занята"})
	l := StateOf(recs, "eeeeeeee")
	if l.State != StateCreated || l.Reason == "" {
		t.Fatalf("заведено, но не отправлено, с причиной; получено %+v", l)
	}
}

func TestWorkingIsNeverInventedWithoutASupervisor(t *testing.T) {
	// Занятость исполнителя про наше поручение не доказывает ничего, и
	// выдавать её за «работает над этим» нельзя. Эти состояния появятся только
	// вместе с наблюдателем, который их и запишет.
	recs := lc(journal.Record{Task: "ffffffff", Event: journal.Started, Pane: "wE:p13"})
	if got := StateOf(recs, "ffffffff").State; got == StateWorking || got == StateProgress {
		t.Fatalf("без наблюдателя эти состояния не выводятся, получено %s", got)
	}
}

func TestOpenListsOnlyWhatIsStillAwaited(t *testing.T) {
	// Ровно то, о чём имеет смысл спрашивать «ну что там».
	recs := lc(
		journal.Record{Task: "11111111", Event: journal.Started, Pane: "a"},
		journal.Record{Task: "22222222", Event: journal.Started, Pane: "b"},
		journal.Record{Task: "22222222", Event: journal.Reported, Outcome: "готово"},
		journal.Record{Task: "22222222", Event: journal.Notified, Stage: StageReported, Outcome: WokeUp},
		journal.Record{Task: "33333333", Event: journal.Started, Pane: "c"},
		journal.Record{Task: "33333333", Event: journal.Reported, Outcome: "готово"},
		journal.Record{Task: "33333333", Event: journal.Notified, Stage: StageReported, Outcome: ToldHuman},
	)
	got := Open(recs)
	if len(got) != 2 {
		t.Fatalf("ждут двое: неотчитавшийся и недоставленный; получено %+v", got)
	}
	states := map[string]string{}
	for _, l := range got {
		states[l.Task] = l.State
	}
	if states["11111111"] != StateSent || states["33333333"] != StateUndelivered {
		t.Fatalf("состояния названы верно, получено %+v", states)
	}
}
