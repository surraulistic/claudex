package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

// pending готовит журнал, в котором отчёт записан, а до разговора не дошёл —
// ровно то состояние, в котором задача 465a77fc провисела ночь.
func pending(t *testing.T, j *journal.Journal, id, thread, report string) {
	t.Helper()
	for _, r := range []journal.Record{
		{Task: id, Event: journal.Started, Pane: "wE:p37", Time: time.Now().Add(-time.Hour),
			Target: thread, TargetSession: thread, TargetKind: KindThread,
			TargetState: "live", TargetSeen: time.Now().Add(-time.Hour)},
		{Task: id, Event: journal.Finished, Pane: "wE:p37", Outcome: "отправлено без ожидания"},
		{Task: id, Event: journal.Reported, Pane: "wE:p37", Outcome: "готово", Reason: report},
		{Task: id, Event: journal.Notified, Target: thread, TargetKind: KindThread,
			Stage: StageReported, Cause: CauseThreadNotLive,
			Outcome: ToldHuman, Reason: "тред закрыт"},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnconfirmableAddressIsLeftAloneUntilItIsKnown(t *testing.T) {
	// Дожимать нечего, пока адрес подтвердить нечем: про этот тред
	// $CODEX_HOME не знает ничего. Отчёт остаётся в канале вытягивания.
	//
	// Прежде так же обходили и закрытый тред — ошибочно: очередь он принимает
	// и читает при следующем пробуждении.
	sent := codexHome(t)
	_, j, c := threadDelivery(t)
	pending(t, j, "465a77fc", unknownThread, "полный текст отчёта")

	got := Flush(context.Background(), FlushOptions{Journal: j, Client: c})
	if len(got) != 0 {
		t.Fatalf("неподтверждённый адрес не трогаем, получено %+v", got)
	}
	if len(*sent) != 0 {
		t.Fatalf("в очередь ничего не клали, получено %+v", *sent)
	}
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatal("отчёт остаётся зависшим")
	}
}

func TestReopenedThreadGetsTheWholeReportOnTheNextTouch(t *testing.T) {
	// Разговор вернулся — и следующий же вызов claudex досылает отчёт.
	// Наблюдателя для этого не нужно: работа делается внутри команды.
	sent := codexHome(t, otherThread)
	_, j, c := threadDelivery(t)
	pending(t, j, "465a77fc", otherThread, "полный текст отчёта")

	got := Flush(context.Background(), FlushOptions{Journal: j, Client: c})
	if len(got) != 1 || !got[0].Ok {
		t.Fatalf("досылка прошла, получено %+v", got)
	}
	if len(*sent) != 1 || (*sent)[0].thread != otherThread {
		t.Fatalf("ушло именно в свой разговор, получено %+v", *sent)
	}
	// В разговор уходит событие, а не отчёт: технической переписке инструмента
	// там не место. Неизменным остаётся другое — разбудили и сказали, где
	// читать, а сам текст цел в журнале.
	msg := (*sent)[0].message
	if strings.Contains(msg, "полный текст отчёта") {
		t.Fatalf("отчёт в диалог не кладётся, получено %q", msg)
	}
	if !strings.Contains(msg, "465a77fc") || !strings.Contains(msg, "claudex task log") {
		t.Fatalf("сказано, что случилось и где читать, получено %q", msg)
	}
	recs2, _ := j.Read()
	var kept bool
	for _, r := range recs2 {
		if r.Event == journal.Reported && strings.Contains(r.Reason, "полный текст отчёта") {
			kept = true
		}
	}
	if !kept {
		t.Fatal("текст отчёта цел в журнале")
	}
	recs, _ := j.Read()
	if l := LostReports(recs); len(l) != 0 {
		t.Fatalf("после досылки зависшего нет, получено %+v", l)
	}
}

func TestFlushNeverWritesIntoANeighbouringConversation(t *testing.T) {
	// Соседний разговор жив, наш закрыт. Отправить туда значило бы прислать
	// ответ тому, кто вопроса не задавал.
	sent := codexHome(t, leaderThread) // жив сосед, адресат закрыт
	_, j, c := threadDelivery(t)
	pending(t, j, "465a77fc", otherThread, "отчёт")

	got := Flush(context.Background(), FlushOptions{Journal: j, Client: c})
	if len(got) != 1 || len(*sent) != 1 {
		t.Fatalf("своему адресату дожали ровно раз, получено %+v %+v", got, *sent)
	}
	if (*sent)[0].thread != otherThread {
		t.Fatalf("писали своему, а не соседу, получено %q", (*sent)[0].thread)
	}
}

func TestFlushDoesNotSendTheSameReportTwice(t *testing.T) {
	sent := codexHome(t, otherThread)
	_, j, c := threadDelivery(t)
	pending(t, j, "465a77fc", otherThread, "отчёт")

	o := FlushOptions{Journal: j, Client: c}
	if got := Flush(context.Background(), o); len(got) != 1 || !got[0].Ok {
		t.Fatalf("первая досылка прошла, получено %+v", got)
	}
	if got := Flush(context.Background(), o); len(got) != 0 {
		t.Fatalf("второй раз то же не шлём, получено %+v", got)
	}
	if len(*sent) != 1 {
		t.Fatalf("сообщение одно, получено %d", len(*sent))
	}
}

func TestFlushLeavesPaneTargetsToTheirOwnPath(t *testing.T) {
	// Панель за час успевает сменить разговор, и проверять это надо в момент
	// доставки, а не по журналу. Дожим панели не трогает.
	sent := codexHome(t, otherThread)
	_, j, c := threadDelivery(t)
	for _, r := range []journal.Record{
		{Task: "aaaaaaaa", Event: journal.Started, Pane: "wE:p37", Target: "wE:p17"},
		{Task: "aaaaaaaa", Event: journal.Finished, Outcome: TimedOut},
		{Task: "aaaaaaaa", Event: journal.Reported, Outcome: "готово", Reason: "текст"},
		{Task: "aaaaaaaa", Event: journal.Notified, Target: "wE:p17",
			Stage: StageReported, Outcome: ToldHuman},
	} {
		j.Append(r)
	}
	if got := Flush(context.Background(), FlushOptions{Journal: j, Client: c}); len(got) != 0 {
		t.Fatalf("панель дожимом не трогаем, получено %+v", got)
	}
	if len(*sent) != 0 {
		t.Fatal("в очередь Codex панель не уезжает")
	}
}

func TestFlushIsBoundedSoAnOrdinaryCommandStaysQuick(t *testing.T) {
	// Накопленная очередь не должна превращать обычный вызов в долгий.
	sent := codexHome(t, otherThread)
	_, j, c := threadDelivery(t)
	for i := 0; i < 12; i++ {
		pending(t, j, "0000000"+string(rune('a'+i)), otherThread, "отчёт")
	}
	got := Flush(context.Background(), FlushOptions{Journal: j, Client: c, Max: 3})
	if len(got) != 3 {
		t.Fatalf("за раз дожимаем не больше предела, получено %d", len(got))
	}
	if len(*sent) != 3 {
		t.Fatalf("и отправок столько же, получено %d", len(*sent))
	}
}

func TestBindingEvidenceIsRecordedAtCreation(t *testing.T) {
	// «Тред был жив» должно быть доказуемо по журналу: к моменту отчёта
	// разговор успевает закрыться, и иначе не отличить недосмотр от честной
	// смены состояния.
	codexHome(t, leaderThread)
	f, j, c := threadDelivery(t)
	f.Reply("agent.get", agent("idle"))

	// Исход самого поручения здесь не важен: проверяется запись о заведении.
	Delegate(context.Background(), Options{
		Client: c, Journal: j, Pane: "wE:p2", Prompt: "задача",
		NotifyThread: leaderThread,
		Timeout:      300 * time.Millisecond, Grace: 10 * time.Millisecond, Self: "/claudex",
	})
	recs, _ := j.Read()
	var start journal.Record
	for _, r := range recs {
		if r.Event == journal.Started {
			start = r
		}
	}
	if start.TargetState != "live" {
		t.Fatalf("состояние адреса записано, получено %q", start.TargetState)
	}
	if start.TargetSeen.IsZero() {
		t.Fatal("записано, когда проверяли")
	}
}

func TestClosedThreadIsFlushedNotPostponedForever(t *testing.T) {
	// Дожим требовал живого треда и пропускал закрытый — а тот очередь
	// принимает и читает при следующем пробуждении. Из-за этого отчёт тому,
	// кто просто закрыл приложение, откладывался вечно.
	sent := codexHome(t) // otherThread известен, замка нет
	_, j, c := threadDelivery(t)
	pending(t, j, "465a77fc", otherThread, "полный текст отчёта")

	got := Flush(context.Background(), FlushOptions{Journal: j, Client: c})
	if len(got) != 1 || !got[0].Ok {
		t.Fatalf("закрытый тред дожимается, получено %+v", got)
	}
	if len(*sent) != 1 || (*sent)[0].thread != otherThread {
		t.Fatalf("ушло в его очередь, получено %+v", *sent)
	}
}

func TestFlushLeavesHopelessReportsAlone(t *testing.T) {
	// Предел за проход — ресурс: пока его занимают адреса, молчащие неделю,
	// свежий отчёт ждёт своей очереди. На живом журнале 34 таких пережили
	// четыре тысячи проходов и не сдвинулись.
	sent := codexHome(t, otherThread) // адресат жив — отказа по адресу не будет
	_, j, c := threadDelivery(t)
	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, r := range []journal.Record{
		{Task: "aaaaaaaa", Event: journal.Started, Pane: "wE:p37", Time: old,
			Target: otherThread, TargetSession: otherThread, TargetKind: KindThread},
		{Task: "aaaaaaaa", Event: journal.Reported, Outcome: "готово", Reason: "старое", Time: old},
		{Task: "aaaaaaaa", Event: journal.Notified, Target: otherThread, TargetKind: KindThread,
			Stage: StageReported, Cause: CauseThreadUnknown, Outcome: ToldHuman, Time: old},
	} {
		j.Append(r)
	}

	got := Flush(context.Background(), FlushOptions{Journal: j, Client: c})
	if len(got) != 0 {
		t.Fatalf("брошенное дожимом не трогаем, получено %+v", got)
	}
	if len(*sent) != 0 {
		t.Fatalf("и в очередь ничего не клали, получено %+v", *sent)
	}
	// Но из виду оно не пропало.
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatal("отчёт по-прежнему виден в недоставленном")
	}
}
