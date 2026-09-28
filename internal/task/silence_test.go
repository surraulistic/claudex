package task

import (
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

func silenceRecs(idleAgo time.Duration, last string, extra ...journal.Record) []journal.Record {
	now := time.Now()
	recs := []journal.Record{
		{Task: "43bc6e11", Event: journal.Started, PaneSession: "разговор",
			Target: "тред", TargetKind: KindThread, Time: now.Add(-time.Hour)},
		{Task: "43bc6e11", Event: journal.Idle, PaneSession: "разговор",
			Reason: last, Time: now.Add(-idleAgo)},
	}
	return append(recs, extra...)
}

func TestSilenceIsNoticedWithoutAnyDoneCall(t *testing.T) {
	// Ради этого хук и заводился: задача может не вызвать done вовсе — 168 из
	// 424 не вызвали, — а затеявший всё равно должен узнать.
	got := SilentTasks(silenceRecs(10*time.Minute, "миграции применены, тесты зелёные"),
		time.Now(), SilenceGrace)
	if len(got) != 1 || got[0].Task != "43bc6e11" {
		t.Fatalf("молчун замечен, получено %+v", got)
	}
	if got[0].Last != "миграции применены, тесты зелёные" {
		t.Fatalf("последняя реплика сохранена, получено %q", got[0].Last)
	}
}

func TestFreshSilenceAfterAStopIsGivenTime(t *testing.T) {
	// Задача может вызвать done через минуту после последней реплики.
	// Собственный отчёт ценнее наблюдения, и перехватывать его незачем.
	if got := SilentTasks(silenceRecs(30*time.Second, "почти всё"), time.Now(), SilenceGrace); len(got) != 0 {
		t.Fatalf("свежему молчанию даём время, получено %+v", got)
	}
}

func TestOwnReportWins(t *testing.T) {
	recs := silenceRecs(time.Hour, "последняя реплика",
		journal.Record{Task: "43bc6e11", Event: journal.Reported, Outcome: "готово"})
	if got := SilentTasks(recs, time.Now(), SilenceGrace); len(got) != 0 {
		t.Fatalf("отчитавшийся молчуном не считается, получено %+v", got)
	}
}

func TestLastIdleWins(t *testing.T) {
	// Ходов у задачи много; важен тот, после которого она замолчала.
	now := time.Now()
	recs := []journal.Record{
		{Task: "43bc6e11", Event: journal.Started, PaneSession: "разговор", Time: now.Add(-time.Hour)},
		{Task: "43bc6e11", Event: journal.Idle, Reason: "первый ход", Time: now.Add(-50 * time.Minute)},
		{Task: "43bc6e11", Event: journal.Idle, Reason: "последний ход", Time: now.Add(-40 * time.Minute)},
	}
	got := SilentTasks(recs, now, SilenceGrace)
	if len(got) != 1 || got[0].Last != "последний ход" {
		t.Fatalf("берётся последняя отметка, получено %+v", got)
	}
}

func TestSilenceEventObservesAndNeverJudges(t *testing.T) {
	// Мы видели, что работа кончилась, но не знаем, чем. Назвать это «готово»
	// значит соврать ровно там, ради чего отчёт и нужен.
	e := SilenceEvent(Silence{Task: "43bc6e11", Last: "ветка запушена, тесты флакают"}, "wE:p13")
	if e.State != StateLost {
		t.Fatalf("состояние — «закончило молча», а не готово; получено %s", e.State)
	}
	txt := e.Text()
	for _, no := range []string{"готово", "провал"} {
		if strings.Contains(txt, no) {
			t.Errorf("оценки в событии нет, а есть %q:\n%s", no, txt)
		}
	}
	if !strings.Contains(txt, "ветка запушена") {
		t.Error("последняя реплика передана как доказательство")
	}
	if !strings.Contains(txt, "claudex task log") {
		t.Error("сказано, где читать подробности")
	}
}

func TestNoEvidenceIsSaidPlainly(t *testing.T) {
	// Хук может не передать реплику. Тогда событие говорит только про
	// молчание и ничего не придумывает.
	e := SilenceEvent(Silence{Task: "43bc6e11"}, "wE:p13")
	if strings.Contains(e.Reason, "последняя реплика") {
		t.Fatalf("доказательства нет — о нём и не заявляем, получено %q", e.Reason)
	}
}

func TestAlreadyWokenIsNotWokenAgain(t *testing.T) {
	// Дедупликация та же, что у отчётов: по паре задача+стадия.
	recs := silenceRecs(time.Hour, "последняя реплика",
		journal.Record{Task: "43bc6e11", Event: journal.Notified,
			Stage: StageReported, Outcome: WokeUp})
	if got := SilentTasks(recs, time.Now(), SilenceGrace); len(got) != 0 {
		t.Fatalf("второй раз за то же не будим, получено %+v", got)
	}
}

func TestFailedWakeIsRetried(t *testing.T) {
	// Неудачное пробуждение дедупликацией не считается: иначе молчун пропал бы
	// навсегда из-за одного закрытого разговора.
	recs := silenceRecs(time.Hour, "последняя реплика",
		journal.Record{Task: "43bc6e11", Event: journal.Notified,
			Stage: StageReported, Outcome: ToldHuman, Cause: CauseThreadNotLive})
	if got := SilentTasks(recs, time.Now(), SilenceGrace); len(got) != 1 {
		t.Fatalf("неудачу повторяем, получено %+v", got)
	}
}
