package task

import (
	"context"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
)

// Дожим зависших отчётов.
//
// Демона у ClauDex нет и заводить его незачем: разговор Codex закрывается и
// открывается снова под тем же идентификатором — замерено, тред 01a0a268 за
// сутки менял состояние четырежды. Значит достаточно проверять адрес в тот
// момент, когда claudex и так запущен.
//
// Отсюда правило: любой вызов claudex дожимает то, что раньше не ушло. Ждать
// нечего, фонового наблюдателя нет, ввод не занят — работа делается внутри уже
// идущей команды и ограничена сверху.
//
// Адрес берётся только записанный при заведении. Соседний живой разговор
// адресом не становится ни при каких условиях: он о поручении не знает, и
// отчёт в нём — мусор, который выглядит как ответ.

const (
	// FlushMax — сколько отчётов дожимать за один вызов. Предел нужен, чтобы
	// накопившаяся очередь не превращала обычную команду в долгую.
	FlushMax = 5
	// FlushBudget — общий предел на дожим.
	FlushBudget = 20 * time.Second
	// flushDeadline — сколько ждать один разговор. Ждать закрытый незачем: он
	// не откроется за эти секунды, а отчёт уже лежит в канале вытягивания.
	flushDeadline = 3 * time.Second
)

type FlushOptions struct {
	Journal *journal.Journal
	Client  *herdr.Client
	Max     int
	Budget  time.Duration
	// FullReport — слать отчёт целиком даже тем, кого можно разбудить событием.
	FullReport bool
	// Skip — поручения, которые на этот раз трогать не надо. Наблюдатель
	// держит здесь адреса, только что отказавшие: повторять попытку каждый
	// тик — шум, а не настойчивость.
	Skip    map[string]bool
	Compose func(summary, task string, from, to time.Time, pane string) string
}

// Flushed — что удалось дожать.
type Flushed struct {
	Task    string `json:"task"`
	Stage   string `json:"stage"`
	Target  string `json:"target"`
	Ok      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

// Flush пытается доставить отчёты, которые до ведущего не дошли.
//
// Возвращает только то, что трогал: молчание значит «дожимать было нечего»
// либо «адреса по-прежнему закрыты».
func Flush(ctx context.Context, o FlushOptions) []Flushed {
	if o.Journal == nil {
		return nil
	}
	if o.Max <= 0 {
		o.Max = FlushMax
	}
	if o.Budget <= 0 {
		o.Budget = FlushBudget
	}
	recs, err := o.Journal.Read()
	if err != nil {
		return nil
	}
	pending := LostReports(recs)
	if len(pending) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, o.Budget)
	defer cancel()

	home := codex.Home()
	var out []Flushed
	for _, l := range pending {
		if len(out) >= o.Max || ctx.Err() != nil {
			break
		}
		if o.Skip[l.Task] {
			// Наблюдатель просит переждать: этот адрес только что отказал, и
			// долбиться в него каждый тик — шум без пользы.
			continue
		}
		st := stateOf(recs, l.Task)
		switch st.kind {
		case KindThread:
			if codex.State(home, l.Target) != codex.Live {
				continue
			}
		case KindSession:
			// Живость разговора — файл в реестре. Дожимать его можно тем же
			// правом, что и тред: адрес есть сам разговор, промахнуться нельзя.
			if _, err := claudesess.Lookup(claudesess.Home(), l.Target); err != nil {
				continue
			}
		default:
			// Панель дожимать нельзя: за час она успевает сменить разговор, и
			// проверять это надо в момент доставки, а не по журналу.
			continue
		}

		var compose func(string) string
		if o.Compose != nil {
			task, from, pane := l.Task, st.started, st.pane
			compose = func(summary string) string {
				return o.Compose(summary, task, from, time.Now(), pane)
			}
		}
		d := Deliver(ctx, o.Client, o.Journal, l.Task, l.Target,
			flushText(l),
			DeliverOptions{Stage: l.Stage, Kind: st.kind, WantSession: l.Target,
				Compose: compose, HumanTold: true, FullReport: o.FullReport,
				Event: &Event{Task: l.Task, Stage: l.Stage, Executor: st.pane,
					State:  StateOf(recs, l.Task).State,
					Reason: "отчёт был готов, но сразу не доставился (" + l.Reason + ")"},
				Deadline: flushDeadline, Poll: 200 * time.Millisecond})

		out = append(out, Flushed{
			Task: l.Task, Stage: l.Stage, Target: l.Target,
			Ok: d.OK, Reason: d.Reason,
		})
	}
	return out
}

// flushText — то, что уедет в разговор. Отчёт идёт целиком: ведущий его ещё не
// видел, и сокращать пересказом нечего.
func flushText(l Lost) string {
	head := "Отложенный отчёт по поручению " + l.Task +
		": доставить сразу не вышло (" + l.Reason + "), разговор открылся — досылаем."
	if l.Report == "" {
		return head
	}
	return head + "\n\n" + l.Report
}
