package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

const hookSession = "7e403273-2034-4296-a833-d176bd30e03d"

func started(id string) journal.Record {
	return journal.Record{Task: id, Event: journal.Started,
		PaneSession: hookSession, Time: time.Now().Add(-time.Hour)}
}

func TestIdleMarkIsSkippedWhenSeveralTasksAreOpen(t *testing.T) {
	// Конец хода не говорит, какое из поручений закончилось. Пометить все
	// значит записать догадку: на живом журнале у одного исполнителя их было
	// 62, и один конец хода разбудил бы затеявшего по каждому.
	recs := []journal.Record{started("aaaaaaaa"), started("bbbbbbbb")}
	if id, ok := idleMark(recs, hookSession, "текст"); ok {
		t.Fatalf("при нескольких открытых не метим, получено %q", id)
	}
}

func TestIdleMarkIsWrittenForTheOnlyOpenTask(t *testing.T) {
	recs := []journal.Record{started("aaaaaaaa")}
	id, ok := idleMark(recs, hookSession, "миграции применены")
	if !ok || id != "aaaaaaaa" {
		t.Fatalf("единственное открытое отмечается, получено %q %v", id, ok)
	}
}

func TestNothingOpenMeansNothingToMark(t *testing.T) {
	if _, ok := idleMark(nil, hookSession, "текст"); ok {
		t.Fatal("без поручений журнал не трогаем — иначе это лог ходов")
	}
}

func TestHeadlessResumeBindsHookSessionToTask(t *testing.T) {
	path := t.TempDir() + "/journal.jsonl"
	j := journal.Open(path)
	initial := journal.Record{Task: "abc12345", Event: journal.Started,
		Headless: true, PaneSession: hookSession, CWD: t.TempDir()}
	if err := j.Append(initial); err != nil {
		t.Fatal(err)
	}
	recs, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEX_TASK_ID", "abc12345")
	resumedID := "6e68e968-4fc0-411f-a41c-d25eb6b6586e"
	if err := bindHeadlessSession(j, recs, resumedID); err != nil {
		t.Fatal(err)
	}
	updated, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got, _, ok := task.HeadlessExecutor(updated, "abc12345"); !ok || got != resumedID {
		t.Fatalf("Stop-hook адресует возобновлённый разговор: %q %v", got, ok)
	}
}

func TestHeadlessBindingIgnoresUnrelatedSession(t *testing.T) {
	path := t.TempDir() + "/journal.jsonl"
	j := journal.Open(path)
	if err := j.Append(journal.Record{Task: "abc12345", Event: journal.Started,
		Headless: true, PaneSession: hookSession}); err != nil {
		t.Fatal(err)
	}
	recs, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Getenv("CLAUDEX_TASK_ID")
	t.Setenv("CLAUDEX_TASK_ID", "other-task")
	t.Cleanup(func() { _ = os.Setenv("CLAUDEX_TASK_ID", old) })
	if err := bindHeadlessSession(j, recs, "6e68e968-4fc0-411f-a41c-d25eb6b6586e"); err != nil {
		t.Fatal(err)
	}
	updated, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 {
		t.Fatalf("чужой hook не меняет привязку: %d записей", len(updated))
	}
}

func TestRepeatedIdenticalMarkIsNotWrittenTwice(t *testing.T) {
	// Ход кончается много раз подряд; повтор того же текста журналу ничего не
	// добавляет — за час их набралось семь по одному поручению.
	recs := []journal.Record{
		started("aaaaaaaa"),
		{Task: "aaaaaaaa", Event: journal.Idle, Reason: "тот же текст"},
	}
	if _, ok := idleMark(recs, hookSession, "тот же текст"); ok {
		t.Fatal("повтор не пишется")
	}
	if id, ok := idleMark(recs, hookSession, "новый текст"); !ok || id != "aaaaaaaa" {
		t.Fatalf("изменившийся текст пишется, получено %q %v", id, ok)
	}
}

var (
	leaderA = task.Wake{Kind: task.KindSession, Target: "aaaaaaaa-0000-4000-8000-00000000000a",
		Session: "aaaaaaaa-0000-4000-8000-00000000000a"}
	leaderB = task.Wake{Kind: task.KindThread, Target: "01a0bbbb-0000-7000-8000-00000000000b",
		Session: "01a0bbbb-0000-7000-8000-00000000000b"}
)

// startedBy — поручение исполнителю hookSession, которое затеял from.
func startedBy(id string, from task.Wake, age time.Duration) journal.Record {
	r := started(id)
	r.Target, r.TargetSession, r.TargetKind = from.Target, from.Session, from.Kind
	r.Time = time.Now().Add(-age)
	return r
}

func TestSendContinuesTheOnlyOpenAssignment(t *testing.T) {
	// Отправка работнику, у которого работа идёт, — продолжение, а не второе
	// поручение: прежде каждая отправка заводила отдельное, и у одного
	// исполнителя их накопилось 62.
	recs := []journal.Record{startedBy("aaaaaaaa", leaderA, time.Hour)}
	id, why, err := continueOrStart(recs, hookSession, false, leaderA)
	if err != nil || id != "aaaaaaaa" || why != "" {
		t.Fatalf("тот же ведущий продолжает своё, получено %q %q %v", id, why, err)
	}
}

func TestSendFromAnotherLeaderStartsItsOwnAssignment(t *testing.T) {
	// R4: ответ на продолжение уходит тому, кто затеял исходное поручение.
	// Чужое продолжение увело бы его прежнему ведущему.
	recs := []journal.Record{startedBy("aaaaaaaa", leaderA, time.Hour)}
	id, why, err := continueOrStart(recs, hookSession, false, leaderB)
	if err != nil || id != "" {
		t.Fatalf("чужое поручение не продолжается, получено %q %v", id, err)
	}
	if !strings.Contains(why, "aaaaaaaa") || !strings.Contains(why, "другой ведущий") {
		t.Fatalf("сказано, почему не продолжение: %q", why)
	}
}

func TestSamePaneWithAnotherConversationIsAnotherLeader(t *testing.T) {
	pane := task.Wake{Kind: task.KindPane, Target: "wE:p1", Session: "s-1"}
	recs := []journal.Record{startedBy("aaaaaaaa", pane, time.Hour)}
	if id, _, _ := continueOrStart(recs, hookSession, false, pane); id != "aaaaaaaa" {
		t.Fatalf("та же панель и тот же разговор — своё, получено %q", id)
	}
	other := pane
	other.Session = "s-2"
	if id, _, _ := continueOrStart(recs, hookSession, false, other); id != "" {
		t.Fatalf("за панелью другой разговор — не своё, получено %q", id)
	}
	other.Session = ""
	if id, _, _ := continueOrStart(recs, hookSession, false, other); id != "" {
		t.Fatalf("разговор в панели не определён — не своё, получено %q", id)
	}
}

func TestAbandonedAssignmentIsNotContinued(t *testing.T) {
	recs := []journal.Record{startedBy("aaaaaaaa", leaderA, 5*24*time.Hour)}
	if st := task.StateOf(recs, "aaaaaaaa"); st.State != task.StateAbandoned {
		t.Fatalf("предпосылка: поручение брошено, получено %s", st.State)
	}
	id, why, err := continueOrStart(recs, hookSession, false, leaderA)
	if err != nil || id != "" || !strings.Contains(why, "брошен") {
		t.Fatalf("брошенное не продолжается даже своим, получено %q %q %v", id, why, err)
	}
}

func TestUnknownSenderIsRefusedNotGuessed(t *testing.T) {
	recs := []journal.Record{startedBy("aaaaaaaa", leaderA, time.Hour)}
	_, _, err := continueOrStart(recs, hookSession, false, task.Wake{})
	var unknown *task.SenderUnknown
	if !errors.As(err, &unknown) || len(unknown.Open) != 1 {
		t.Fatalf("чьё продолжение — не угадать, получено %v", err)
	}
	if !strings.Contains(err.Error(), "claudex send <id поручения>") || !strings.Contains(err.Error(), "--new") {
		t.Fatalf("названы оба выхода: %v", err)
	}
	if _, _, err := continueOrStart(nil, hookSession, false, task.Wake{}); err != nil {
		t.Fatalf("открытого нет — заводим новое и без отправителя: %v", err)
	}
}

func TestSendStartsFreshWhenNothingIsOpen(t *testing.T) {
	id, why, err := continueOrStart(nil, hookSession, false, leaderA)
	if err != nil || id != "" || why != "" {
		t.Fatalf("работы нет — заводим новое, получено %q %q %v", id, why, err)
	}
}

func TestNewFlagForcesASeparateAssignment(t *testing.T) {
	recs := []journal.Record{startedBy("aaaaaaaa", leaderA, time.Hour)}
	id, _, err := continueOrStart(recs, hookSession, true, leaderA)
	if err != nil || id != "" {
		t.Fatalf("--new заводит отдельное, получено %q %v", id, err)
	}
	if id, _, err := continueOrStart(recs, hookSession, true, task.Wake{}); err != nil || id != "" {
		t.Fatalf("--new не спрашивает отправителя, получено %q %v", id, err)
	}
}

func TestSeveralOpenAssignmentsRefuseRatherThanPick(t *testing.T) {
	recs := []journal.Record{startedBy("aaaaaaaa", leaderA, time.Hour), startedBy("bbbbbbbb", leaderA, time.Hour),
		startedBy("cccccccc", leaderB, time.Hour)}
	_, _, err := continueOrStart(recs, hookSession, false, leaderA)
	var many *task.TooManyOpen
	if !errors.As(err, &many) {
		t.Fatalf("какое продолжают — знает только человек, получено %v", err)
	}
	if len(many.Open) != 2 {
		t.Fatalf("названы оба своих, чужое не в счёт, получено %+v", many.Open)
	}
	if id, _, err := continueOrStart(recs, hookSession, false, leaderB); err != nil || id != "cccccccc" {
		t.Fatalf("у второго ведущего своё ровно одно, получено %q %v", id, err)
	}
}

func TestIdleMarkStopsAfterTheTerm(t *testing.T) {
	// R8: долгоживущий разговор с одним открытым поручением не растит журнал
	// репликой на каждый ход после того, как молчание уже никого не будит.
	old := started("aaaaaaaa")
	old.Time = time.Now().Add(-task.AbandonAfter - time.Hour)
	if _, ok := idleMark([]journal.Record{old}, hookSession, "реплика"); ok {
		t.Fatal("старше срока отметка не пишется")
	}
	if id, ok := idleMark([]journal.Record{started("aaaaaaaa")}, hookSession, "реплика"); !ok || id != "aaaaaaaa" {
		t.Fatalf("свежее поручение отмечается, получено %q %v", id, ok)
	}
}
