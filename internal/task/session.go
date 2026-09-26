package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
)

// Доставка в разговор Claude Code.
//
// Третий адрес рядом с панелью и тредом Codex. Заведён ради того же, ради чего
// заводился тред: панель переживает смену агента, и «панель свободна»
// разговора не доказывает — отсюда взялась проверка
// executor_conversation_changed. У сокета промахнуться разговором нельзя по
// устройству: адрес и есть разговор.
//
// Ждать, как панель, здесь нечего. Занятый разговор принимает сообщение и в
// середине хода, а прочитает его между вызовами инструментов — ровно как
// очередь треда.

const (
	// CauseSessionGone — разговор был записан при заведении, а сейчас его
	// среди живых нет. Сессия могла кончиться между поручением и отчётом.
	CauseSessionGone = "conversation_gone"
	// CauseSessionAmbiguous — под имя подходит несколько живых разговоров.
	CauseSessionAmbiguous = "conversation_ambiguous"
	// CauseSendFailed — сокет не принял сообщение.
	CauseSendFailed = "session_send_failed"
)

func deliverSession(ctx context.Context, c *herdr.Client, j *journal.Journal,
	id, target, text string, o DeliverOptions) Delivery {

	started := time.Now()
	d := Delivery{Target: target}

	fail := func(cause, reason string) Delivery {
		d.Waited = time.Since(started)
		d.Seconds = int(d.Waited.Seconds())
		d.Cause, d.Reason = cause, reason
		if !o.HumanTold {
			d.Fallback, d.Refused = tellHuman(c, "claudex: поручение "+id+" завершено",
				text+"\n\n"+d.Reason)
		}
		record(j, id, target, KindSession, o.Stage, d)
		return d
	}

	// Адрес пришёл из журнала, а не от человека: собственный разговор здесь
	// законный адресат, и отбрасывать его нельзя.
	s, err := claudesess.Lookup(claudesess.Home(), target)
	if err != nil {
		var amb *claudesess.Ambiguous
		switch {
		case errors.As(err, &amb):
			return fail(CauseSessionAmbiguous, amb.Error())
		default:
			return fail(CauseSessionGone,
				fmt.Sprintf("разговор %s недоступен: %v", short(target), err))
		}
	}

	queued := text
	if o.Compose != nil {
		queued = o.Compose(text)
	}
	if err := claudesess.Send(ctx, s, queued); err != nil {
		return fail(CauseSendFailed, err.Error())
	}

	d.OK = true
	d.Waited = time.Since(started)
	d.Seconds = int(d.Waited.Seconds())
	record(j, id, target, KindSession, o.Stage, d)
	return d
}

// WithReport — тот же текст поручения с припиской, как отчитаться. Нужен
// ветке доставки в разговор: у неё нет панели, но обязанность отчитаться та же.
// Пустой self читается как «тот же бинарь, что поручает»: на PATH может
// лежать другая сборка, которая про `done` не знает.
func WithReport(prompt, self, id string) string {
	if self == "" {
		self = selfPath()
	}
	return withReportInstruction(prompt, self, id)
}

// SentWithoutWaiting отмечает, что поручение отправлено и наблюдателя за ним
// нет.
//
// Без этой записи Report считает, что отчёт доставит наблюдатель, и доставку
// пропускает — а наблюдателя при `--no-wait` не существует. Отчёт тогда
// записан, но никуда не уходит и даже в `undelivered` не виден: молчаливая
// потеря ровно того вида, ради предотвращения которого канал вытягивания и
// заводился. Поймано smoke-тестом на поручении 28f2b0ba.
//
// Отдельной функцией, потому что тот же смысл нужен обеим веткам — панельной и
// сессионной, — а повторённая в двух местах строка и дала расхождение.
func SentWithoutWaiting(j *journal.Journal, id, pane string) error {
	if j == nil {
		return nil
	}
	return j.Append(journal.Record{
		Task: id, Event: journal.Finished, Pane: pane,
		Outcome: SentNoWait,
	})
}

// SentNoWait — исход отправки без ожидания.
const SentNoWait = "отправлено без ожидания"
