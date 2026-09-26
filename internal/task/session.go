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

	s, err := claudesess.Resolve(claudesess.Home(), target)
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
