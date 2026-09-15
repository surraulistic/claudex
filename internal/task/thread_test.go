package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
	"github.com/surraulistic/claudex/internal/journal"
)

const (
	leaderThread = "01a0a51e-20c4-7e10-8488-b524b9389489"
	otherThread  = "01a0a508-1a8d-7ff1-b2b3-47b5e6ceb96b"
)

type queued struct{ thread, message string }

// codexHome подставляет поддельный $CODEX_HOME и перехватывает очередь.
// Настоящий вызов разбудил бы живой разговор, и правило отказа им не проверишь.
func codexHome(t *testing.T, live ...string) *[]queued {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "thread-writer-locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	var index []byte
	for _, id := range live {
		if err := os.WriteFile(filepath.Join(dir, "thread-writer-locks", id+".lock"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		index = append(index, []byte(`{"id":"`+id+`"}`+"\n")...)
	}
	// Закрытый, но известный тред нужен почти каждому тесту как контрпример.
	index = append(index, []byte(`{"id":"`+otherThread+`"}`+"\n")...)
	if err := os.WriteFile(filepath.Join(dir, "session_index.jsonl"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", dir)

	var sent []queued
	prev := codex.Queue
	codex.Queue = func(_ context.Context, id, msg string) error {
		sent = append(sent, queued{id, msg})
		return nil
	}
	t.Cleanup(func() { codex.Queue = prev })
	return &sent
}

func threadDelivery(t *testing.T) (*herdrtest.Fake, *journal.Journal, *herdr.Client) {
	t.Helper()
	f := herdrtest.Start(t)
	f.Reply("agent.get", agentWith("idle", leaderSession))
	f.Reply("agent.prompt", agent("idle"))
	f.Reply("notification.show", `{"id":"x","result":{"shown":true}}`)
	j := journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	return f, j, herdr.New(f.Path)
}

func threadOpts() DeliverOptions {
	return DeliverOptions{Kind: KindThread, Stage: StageReported,
		Deadline: time.Second, Poll: time.Millisecond,
		QueueAttempts: 2, QueueGap: time.Millisecond}
}

func TestThreadDeliveryAddressesTheConversationItself(t *testing.T) {
	sent := codexHome(t, leaderThread)
	f, j, c := threadDelivery(t)

	d := Deliver(context.Background(), c, j, "11111111", leaderThread, "готово", threadOpts())
	if !d.OK {
		t.Fatalf("живой тред принимает очередь, получено %+v", d)
	}
	if len(*sent) != 1 || (*sent)[0].thread != leaderThread {
		t.Fatalf("очередь ушла именно тому треду, получено %+v", *sent)
	}
	if (*sent)[0].message != "готово" {
		t.Fatalf("текст отчёта не подменяется, получено %q", (*sent)[0].message)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("панель тут ни при чём: в herdr с промптом не ходили")
	}
}

func TestThreadDeliverySurvivesPaneReuse(t *testing.T) {
	// Ровно тот сбой, ради которого всё затевалось. Панель ведущего за час
	// сменила разговор; панельная доставка тут отказывает, а адресация по
	// треду попадает туда же, куда и до смены: адрес и есть разговор.
	sent := codexHome(t, leaderThread)
	f, j, c := threadDelivery(t)
	f.Reply("agent.get", agentWith("idle", "совсем-другой-разговор"))
	// Журнал видел за этой панелью несколько разговоров — для панели это
	// приговор (pane_hosts_several_conversations), для треда безразлично.
	j.Append(journal.Record{Task: "т0", Event: journal.Started,
		Target: "wE:p17", TargetSession: "разговор-А"})
	j.Append(journal.Record{Task: "т0", Event: journal.Started,
		Target: "wE:p17", TargetSession: "разговор-Б"})

	d := Deliver(context.Background(), c, j, "11111111", leaderThread, "готово", threadOpts())
	if !d.OK {
		t.Fatalf("смена разговора в панели треду не мешает, получено %+v", d)
	}
	if len(*sent) != 1 || (*sent)[0].thread != leaderThread {
		t.Fatalf("очередь ушла тому же треду, получено %+v", *sent)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("в чужой разговор панели не писали")
	}
}

func TestThreadDeliveryRefusesAClosedThread(t *testing.T) {
	// Закрытый тред принял бы очередь молча и не прочитал её никогда. Это
	// отказ, а не успех: иначе отчёт исчезает, а журнал говорит «доставлено».
	sent := codexHome(t, leaderThread)
	f, j, c := threadDelivery(t)

	d := Deliver(context.Background(), c, j, "11111111", otherThread, "готово", threadOpts())
	if d.OK {
		t.Fatal("в закрытый тред класть нельзя")
	}
	if d.Cause != CauseThreadNotLive {
		t.Fatalf("причина машиночитаема, получено %q", d.Cause)
	}
	if !d.Fallback {
		t.Fatalf("вместо этого говорим человеку, получено %+v", d)
	}
	if len(*sent) != 0 {
		t.Fatalf("очередь не трогали, получено %+v", *sent)
	}
	if f.LastRequest("notification.show") == nil {
		t.Fatal("человеку показано уведомление")
	}
}

func TestThreadDeliveryRefusesAnUnknownThread(t *testing.T) {
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)

	d := Deliver(context.Background(), c, j, "11111111",
		"01a0ffff-0000-7000-8000-000000000000", "готово", threadOpts())
	if d.OK {
		t.Fatal("неизвестный тред — это «подтвердить нечем», а не «наверное, тот же»")
	}
	if d.Cause != CauseThreadUnknown {
		t.Fatalf("причина машиночитаема, получено %q", d.Cause)
	}
	if !d.Fallback || len(*sent) != 0 {
		t.Fatalf("человеку сказали, очередь не трогали: %+v %+v", d, *sent)
	}
}

func TestThreadDeliveryRefusesAMissingBinding(t *testing.T) {
	// Привязки нет вовсе — поручение заводили без CODEX_THREAD_ID. Догадаться
	// о треде по чему-либо ещё нельзя, и делать вид, что можно, тоже.
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)

	d := Deliver(context.Background(), c, j, "11111111", "", "готово", threadOpts())
	if d.OK {
		t.Fatal("без привязки доставлять некуда")
	}
	if d.Cause != CauseUnconfirmedBinding {
		t.Fatalf("причина машиночитаема, получено %q", d.Cause)
	}
	if !strings.Contains(d.Reason, "нечем") {
		t.Fatalf("причина названа словами, получено %q", d.Reason)
	}
	if len(*sent) != 0 {
		t.Fatalf("очередь не трогали, получено %+v", *sent)
	}
}

func TestThreadDeliveryRefusesASessionName(t *testing.T) {
	// `codex queue` принял бы имя, но в указателе сессий три треда подряд
	// зовутся «license service»: доставка по имени — это промах мимо разговора.
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)

	d := Deliver(context.Background(), c, j, "11111111", "license service", "готово", threadOpts())
	if d.OK {
		t.Fatal("имя адресом быть не может")
	}
	if d.Cause != CauseUnconfirmedBinding {
		t.Fatalf("причина машиночитаема, получено %q", d.Cause)
	}
	if !strings.Contains(d.Reason, "не идентификатор разговора") {
		t.Fatalf("причина названа словами, получено %q", d.Reason)
	}
	if len(*sent) != 0 {
		t.Fatalf("очередь не трогали, получено %+v", *sent)
	}
}

func TestQueueFailureIsNotCalledDelivered(t *testing.T) {
	// Ненулевой код возврата чужой команды — не доставка. Раньше такой исход
	// лёг бы в журнал как успех, и потеря стала бы невидимой.
	codexHome(t, leaderThread)
	f, j, c := threadDelivery(t)
	prev := codex.Queue
	codex.Queue = func(context.Context, string, string) error {
		return errors.New("app-server не отвечает")
	}
	defer func() { codex.Queue = prev }()

	d := Deliver(context.Background(), c, j, "11111111", leaderThread, "готово", threadOpts())
	if d.OK {
		t.Fatal("очередь не приняла — это отказ")
	}
	if d.Cause != CauseQueueFailed {
		t.Fatalf("причина машиночитаема, получено %q", d.Cause)
	}
	if !d.Fallback {
		t.Fatalf("человеку показано, получено %+v", d)
	}
	if f.LastRequest("notification.show") == nil {
		t.Fatal("запасной канал сработал")
	}
	recs, _ := j.Read()
	if len(recs) != 1 || recs[0].Outcome != ToldHuman {
		t.Fatalf("исход виден в журнале, получено %+v", recs)
	}
}

func TestThreadDeliveryOutcomeLandsInTheJournal(t *testing.T) {
	codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)

	Deliver(context.Background(), c, j, "11111111", leaderThread, "готово", threadOpts())
	recs, _ := j.Read()
	if len(recs) != 1 {
		t.Fatalf("одна запись о доставке, получено %+v", recs)
	}
	if recs[0].Event != journal.Notified || recs[0].Outcome != WokeUp {
		t.Fatalf("успех записан как пробуждение, получено %+v", recs[0])
	}
	if recs[0].Target != leaderThread || recs[0].Stage != StageReported {
		t.Fatalf("адрес и стадия записаны, получено %+v", recs[0])
	}
	// Вид адреса нужен и здесь, а не только в записи о заведении: иначе по
	// журналу не отличить доставку в тред от доставки в панель, а именно по
	// нему и разбирают, куда уехал отчёт.
	if recs[0].TargetKind != KindThread {
		t.Fatalf("доставка в тред помечена как тред, получено %q", recs[0].TargetKind)
	}
}

func TestLateThreadReportKeepsTheBindingFromDelegationTime(t *testing.T) {
	// Поздний отчёт приходит спустя час. Вид адреса берётся из журнала, а не
	// из окружения: к этому времени процесс может выполняться где угодно.
	sent := codexHome(t, leaderThread)
	f, j, c := threadDelivery(t)
	j.Append(journal.Record{Task: "11111111", Event: journal.Started, Pane: "wE:p13",
		Target: leaderThread, TargetSession: leaderThread, TargetKind: KindThread})
	j.Append(journal.Record{Task: "11111111", Event: journal.Finished, Outcome: TimedOut})

	res, err := Report(context.Background(), "11111111", "готово", "поздний отчёт",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second, Poll: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Delivered == nil || !res.Delivered.OK {
		t.Fatalf("поздний отчёт доставлен в тот же тред, получено %+v", res.Delivered)
	}
	if len(*sent) != 1 || (*sent)[0].thread != leaderThread {
		t.Fatalf("очередь ушла треду из журнала, получено %+v", *sent)
	}
	if f.LastRequest("agent.prompt") != nil {
		t.Fatal("панель не трогали")
	}
}

func TestRepeatedDoneDoesNotQueueTheThreadTwice(t *testing.T) {
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)
	j.Append(journal.Record{Task: "11111111", Event: journal.Started, Pane: "wE:p13",
		Target: leaderThread, TargetSession: leaderThread, TargetKind: KindThread})
	j.Append(journal.Record{Task: "11111111", Event: journal.Finished, Outcome: TimedOut})

	o := ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second, Poll: time.Millisecond}
	Report(context.Background(), "11111111", "готово", "первый", o)
	res, _ := Report(context.Background(), "11111111", "готово", "второй", o)

	if res.Skipped == "" {
		t.Fatalf("второй done никого не будит, получено %+v", res)
	}
	if len(*sent) != 1 {
		t.Fatalf("в очередь положили один раз, получено %+v", *sent)
	}
}

func TestFailedThreadDeliveryIsRetriedOnTheNextDone(t *testing.T) {
	// Провалившуюся доставку повторить стоит, удавшуюся — нет. Иначе отчёт,
	// не дошедший с первого раза, теряется навсегда.
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)
	j.Append(journal.Record{Task: "11111111", Event: journal.Started, Pane: "wE:p13",
		Target: leaderThread, TargetSession: leaderThread, TargetKind: KindThread})
	j.Append(journal.Record{Task: "11111111", Event: journal.Finished, Outcome: TimedOut})
	j.Append(journal.Record{Task: "11111111", Event: journal.Notified, Target: leaderThread,
		Stage: StageReported, Outcome: NotDelivered})

	res, _ := Report(context.Background(), "11111111", "готово", "повтор",
		ReportOptions{Client: c, Journal: j, Pane: "wE:p13", Deadline: time.Second, Poll: time.Millisecond})
	if res.Delivered == nil || !res.Delivered.OK {
		t.Fatalf("повторили и дошли, получено %+v", res.Delivered)
	}
	if len(*sent) != 1 {
		t.Fatalf("одна отправка, получено %+v", *sent)
	}
}

func TestResolveWakePrefersTheThreadOverThePane(t *testing.T) {
	// Обе переменные выставлены разом у Codex, запущенного в панели herdr.
	// Панель тогда адресом быть не должна: за ней несколько разговоров.
	codexHome(t, leaderThread)
	asCodexChild(t, leaderThread)
	t.Setenv("HERDR_PANE_ID", "wE:p17")

	w := ResolveWake(nil, "", "")
	if w.Kind != KindThread || w.Target != leaderThread {
		t.Fatalf("свой тред старше своей панели, получено %+v", w)
	}
	if w.Session != leaderThread {
		t.Fatalf("у треда адрес и разговор — одно, получено %+v", w)
	}
}

func TestResolveWakeHonoursExplicitNamesFirst(t *testing.T) {
	codexHome(t, leaderThread)
	asCodexChild(t, leaderThread)
	t.Setenv("HERDR_PANE_ID", "wE:p17")

	if w := ResolveWake(nil, "", otherThread); w.Target != otherThread || w.Kind != KindThread {
		t.Fatalf("--notify-thread старше окружения, получено %+v", w)
	}
	if w := ResolveWake(nil, "wE:p99", ""); w.Target != "wE:p99" || w.Kind != KindPane {
		t.Fatalf("--notify старше окружения, получено %+v", w)
	}
}

// asCodexChild выдаёт процесс за потомка названного треда. Сам прогон тестов
// запущен из-под Claude Code, а его переменные как раз и гасят унаследованный
// идентификатор, — их приходится снимать.
func asCodexChild(t *testing.T, thread string) {
	t.Helper()
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", thread)
}

func TestResolveWakeIgnoresAThreadInheritedByClaudeCode(t *testing.T) {
	// Codex запустил Claude Code, тот унаследовал CODEX_THREAD_ID. Ведущий
	// здесь — панель, а не дед по процессу.
	codexHome(t, leaderThread)
	t.Setenv("CODEX_THREAD_ID", leaderThread)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("HERDR_PANE_ID", "wE:p17")

	if w := ResolveWake(nil, "", ""); w.Kind != KindPane || w.Target != "wE:p17" {
		t.Fatalf("унаследованный тред адресом не считается, получено %+v", w)
	}
}

func TestResolveWakeFallsBackToThePaneWithoutACodexThread(t *testing.T) {
	// Ведущий — Claude Code, а не Codex: тред неоткуда взять, и панель
	// остаётся единственным адресом. Прежнее поведение не меняется.
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("HERDR_PANE_ID", "wE:p17")

	w := ResolveWake(nil, "", "")
	if w.Kind != KindPane || w.Target != "wE:p17" {
		t.Fatalf("без треда адресуем панелью, получено %+v", w)
	}
}

func TestWakeOfReadsOldRecordsAsPanes(t *testing.T) {
	// Журнал заведён до появления адресации по треду: пустой вид — панель.
	if w := WakeOf("", "wE:p17", "разговор-А"); w.Kind != KindPane {
		t.Fatalf("старая запись читается как панель, получено %+v", w)
	}
}

func TestPaneDeliveryIsUnchangedWhenKindIsAbsent(t *testing.T) {
	// Опции без Kind ведут себя ровно как раньше: вся панельная ветка должна
	// остаться нетронутой, иначе починка одного сломала бы другое.
	f, j, c := threadDelivery(t)
	d := Deliver(context.Background(), c, j, "11111111", "wE:p17", "готово",
		DeliverOptions{Stage: StageReported, WantSession: leaderSession,
			Deadline: time.Second, Poll: 10 * time.Millisecond})
	if !d.OK {
		t.Fatalf("панельная доставка работает как прежде, получено %+v", d)
	}
	if f.LastRequest("agent.prompt") == nil {
		t.Fatal("сообщение ушло в панель")
	}
	recs, _ := j.Read()
	if len(recs) != 1 || recs[0].TargetKind != KindPane {
		t.Fatalf("панельная доставка помечена как панель, получено %+v", recs)
	}
}

func TestComposeEnrichesTheQueuedMessageOnly(t *testing.T) {
	// Ведущему в тред уходит сводка вместе с дайджестом. Человеку во
	// всплывашку herdr — только сводка: уведомление на пол-экрана бесполезно.
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)
	o := threadOpts()
	o.Compose = func(s string) string { return s + " + ДАЙДЖЕСТ" }

	d := Deliver(context.Background(), c, j, "11111111", leaderThread, "готово", o)
	if !d.OK {
		t.Fatalf("доставка прошла, получено %+v", d)
	}
	if len(*sent) != 1 || (*sent)[0].message != "готово + ДАЙДЖЕСТ" {
		t.Fatalf("в очередь ушёл дайджест, получено %+v", *sent)
	}
}

func TestDirectCallbackWithoutComposeIsUnchanged(t *testing.T) {
	// Обратная совместимость: без индекса и вне Claude-сессии дайджест собрать
	// не из чего, и прямой callback обязан работать ровно как раньше.
	sent := codexHome(t, leaderThread)
	_, j, c := threadDelivery(t)

	d := Deliver(context.Background(), c, j, "11111111", leaderThread, "готово", threadOpts())
	if !d.OK || len(*sent) != 1 || (*sent)[0].message != "готово" {
		t.Fatalf("сводка уходит как есть, получено %+v %+v", d, *sent)
	}
}

func TestRefusedThreadShowsTheHumanTheSummaryNotTheDigest(t *testing.T) {
	// Тред закрыт, отчёт идёт человеку. Дайджест сюда подмешивать нельзя.
	codexHome(t) // ни одного живого замка
	f, j, c := threadDelivery(t)
	o := threadOpts()
	o.Compose = func(s string) string { return s + " + ДАЙДЖЕСТ" }

	d := Deliver(context.Background(), c, j, "11111111", leaderThread, "готово", o)
	if d.OK {
		t.Fatal("закрытый тред не принимает очередь")
	}
	req := f.LastRequest("notification.show")
	if req == nil {
		t.Fatal("человеку сказали")
	}
	if strings.Contains(fmt.Sprint(req), "ДАЙДЖЕСТ") {
		t.Fatal("во всплывашку herdr дайджест не уезжает")
	}
}
