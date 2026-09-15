package task

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/journal"
)

// mute глушит настоящую очередь Codex на время теста. Без этого проверка
// доставки уходит в живой разговор: адрес в журнале — не выдумка.
func mute(t *testing.T) {
	t.Helper()
	prev := codex.Queue
	codex.Queue = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { codex.Queue = prev })
}

// Два случая ниже сняты с живого журнала 15.09 и воспроизводятся дословно:
// именно из-за них Codex не проснулся по нужным поручениям.

const (
	stalePane  = "wE:p13"
	staleOld   = "d7068ca3" // закрыт утром, но сессия помнила его весь день
	staleFresh = "131bbea4" // настоящее поручение, оставшееся без отчёта
	textPane   = "wE:p35"
	textTask   = "be2f3b21"
)

func at(h, m int) time.Time {
	return time.Date(2026, 9, 15, h, m, 0, 0, time.Local)
}

// compactedSession — журнал панели, пережившей компакт: старое поручение
// закрыто утром, новое заведено вечером и ещё не отчиталось.
func compactedSession() []journal.Record {
	return []journal.Record{
		{Task: staleOld, Event: journal.Started, Pane: stalePane, Time: at(10, 10),
			Target: "00000000-0000-7000-8000-000000000000", TargetKind: KindThread},
		{Task: staleOld, Event: journal.Reported, Pane: stalePane, Time: at(10, 11)},
		{Task: staleOld, Event: journal.Finished, Pane: stalePane, Time: at(10, 11)},
		{Task: staleFresh, Event: journal.Started, Pane: stalePane, Time: at(22, 40),
			Target: "00000000-0000-7000-8000-000000000000", TargetKind: KindThread},
		{Task: staleFresh, Event: journal.Finished, Pane: stalePane, Time: at(22, 40)},
	}
}

func TestStaleIDFromACompactedSessionGoesToTheLiveTask(t *testing.T) {
	// 131bbea4 заведена в 22:40, а отчёт в 22:43 пришёл под d7068ca3 —
	// идентификатором, закрытым ещё в 10:11. Настоящее поручение осталось без
	// отчёта, и ведущий не проснулся.
	r, err := Resolve(compactedSession(), stalePane, staleOld)
	if err != nil {
		t.Fatalf("исправление возможно, отказывать нельзя: %v", err)
	}
	if r.Task != staleFresh {
		t.Fatalf("отчёт принадлежит %s, получено %s", staleFresh, r.Task)
	}
	if !r.Corrected || r.Cause != ClaimClosed {
		t.Fatalf("исправление помечено и названо, получено %+v", r)
	}
	if r.Claimed != staleOld {
		t.Fatalf("названный идентификатор сохранён для разбора, получено %q", r.Claimed)
	}
}

func TestReportTextInsteadOfAnIDGoesToTheLiveTask(t *testing.T) {
	// Панель wE:p35 дважды вызвала done, поставив первым доводом текст отчёта.
	// В журнале из-за этого появились записи, где task — целый абзац.
	recs := []journal.Record{
		{Task: textTask, Event: journal.Started, Pane: textPane, Time: at(22, 6),
			Target: "00000000-0000-7000-8000-000000000000", TargetKind: KindThread},
		{Task: textTask, Event: journal.Finished, Pane: textPane, Time: at(22, 6)},
	}
	report := "Догнал корневую причину minio:fail на stage: не бакет и не креды, а бюджет health-чекера"

	r, err := Resolve(recs, textPane, report)
	if err != nil {
		t.Fatalf("исправление возможно: %v", err)
	}
	if r.Task != textTask {
		t.Fatalf("отчёт принадлежит %s, получено %q", textTask, r.Task)
	}
	if r.Cause != ClaimNotID {
		t.Fatalf("причина названа как текст вместо id, получено %q", r.Cause)
	}
	if strings.Contains(r.Task, " ") {
		t.Fatal("идентификатором не может стать абзац")
	}
}

func TestNoIDAtAllIsAcceptedWhenExactlyOneTaskIsLive(t *testing.T) {
	recs := []journal.Record{
		{Task: textTask, Event: journal.Started, Pane: textPane, Time: at(22, 6)},
		{Task: textTask, Event: journal.Finished, Pane: textPane, Time: at(22, 6)},
	}
	r, err := Resolve(recs, textPane, "")
	if err != nil {
		t.Fatalf("одно активное поручение — принимаем: %v", err)
	}
	if r.Task != textTask || r.Cause != ClaimNone {
		t.Fatalf("получено %+v", r)
	}
}

func TestCorrectIDIsAcceptedUntouched(t *testing.T) {
	r, err := Resolve(compactedSession(), stalePane, staleFresh)
	if err != nil {
		t.Fatal(err)
	}
	if r.Task != staleFresh || r.Corrected {
		t.Fatalf("верный идентификатор не исправляют, получено %+v", r)
	}
}

func TestTaskOfAnotherPaneIsNotAccepted(t *testing.T) {
	// Чужое активное поручение — не адрес: панели ведут разную работу, и
	// подписать отчёт чужим идентификатором значит увести его к другому ведущему.
	recs := append(compactedSession(),
		journal.Record{Task: "aaaaaaaa", Event: journal.Started, Pane: "wE:p35", Time: at(22, 6)})
	r, err := Resolve(recs, stalePane, "aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if r.Task != staleFresh || r.Cause != ClaimForeign {
		t.Fatalf("чужое поручение отклонено и исправлено, получено %+v", r)
	}
}

func TestNothingLiveAndNothingKnownIsRefused(t *testing.T) {
	recs := compactedSession()[:3] // только закрытое утреннее
	if _, err := Resolve(recs, stalePane, "ffffffff"); err == nil {
		t.Fatal("чужой идентификатор без живых поручений — отказ")
	}
	if _, err := Resolve(recs, stalePane, "текст отчёта"); err == nil {
		t.Fatal("текст без живых поручений — отказ")
	}
}

func TestRepeatedDoneOnOwnClosedTaskStaysOnIt(t *testing.T) {
	// Повторный done — это повтор доставки, которая никого не достигла.
	// Точный собственный идентификатор догадкой не является.
	recs := compactedSession()[:3]
	r, err := Resolve(recs, stalePane, staleOld)
	if err != nil {
		t.Fatalf("повтор принимается: %v", err)
	}
	if r.Task != staleOld || r.Corrected {
		t.Fatalf("остаётся на своём поручении, получено %+v", r)
	}
}

func TestSeveralLiveTasksAreRefusedRatherThanGuessed(t *testing.T) {
	recs := append(compactedSession(),
		journal.Record{Task: "bbbbbbbb", Event: journal.Started, Pane: stalePane, Time: at(22, 50)})
	_, err := Resolve(recs, stalePane, staleOld)
	if err == nil {
		t.Fatal("две активные — угадывать нельзя")
	}
	for _, want := range []string{staleFresh, "bbbbbbbb"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("в ошибке названы кандидаты, получено %q", err)
		}
	}
}

func TestReportingClosesTheAssignment(t *testing.T) {
	// Повтор не должен спамить: после отчёта поручение перестаёт быть активным.
	recs := compactedSession()
	if len(ActiveFor(recs, stalePane)) != 1 {
		t.Fatalf("до отчёта активна одна, получено %d", len(ActiveFor(recs, stalePane)))
	}
	recs = append(recs, journal.Record{
		Task: staleFresh, Event: journal.Reported, Pane: stalePane, Time: at(22, 43)})
	if n := len(ActiveFor(recs, stalePane)); n != 0 {
		t.Fatalf("после отчёта активных нет, получено %d", n)
	}
	// Живое поручение закрыто, поэтому устаревший идентификатор больше никого
	// не уводит: он остаётся на своём, а не захватывает соседнее.
	r, err := Resolve(recs, stalePane, staleOld)
	if err != nil || r.Task != staleOld {
		t.Fatalf("повтор остаётся на своём поручении, получено %+v %v", r, err)
	}
}

func TestRefusedReportWritesNothingToTheJournal(t *testing.T) {
	// Прежний порядок был обратным: запись появлялась до проверок, и поэтому в
	// журнале осели отчёты под несуществующим идентификатором.
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	for _, r := range compactedSession()[:3] {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := j.Read()

	_, err := Report(context.Background(), "ffffffff", "готово", "текст",
		ReportOptions{Journal: j, Pane: stalePane})
	if err == nil {
		t.Fatal("отказ, а не запись")
	}
	after, _ := j.Read()
	if len(after) != len(before) {
		t.Fatalf("журнал не тронут: было %d, стало %d", len(before), len(after))
	}
}

func TestCorrectedReportLandsUnderTheLiveTaskAndSaysSo(t *testing.T) {
	mute(t)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	for _, r := range compactedSession() {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Report(context.Background(), staleOld, "готово", "текст",
		ReportOptions{Journal: j, Pane: stalePane})
	if err != nil {
		t.Fatal(err)
	}
	if res.Task != staleFresh {
		t.Fatalf("отчёт лёг под живое поручение, получено %s", res.Task)
	}

	recs, _ := j.Read()
	var last journal.Record
	for _, r := range recs {
		if r.Event == journal.Reported && r.Task == staleFresh {
			last = r
		}
	}
	if last.Task != staleFresh {
		t.Fatalf("запись под живым поручением не найдена, получено %+v", recs)
	}
	if last.ClaimedTask != staleOld || last.Correction == "" {
		t.Fatalf("исправление видно в журнале, получено claimed=%q correction=%q",
			last.ClaimedTask, last.Correction)
	}
}
