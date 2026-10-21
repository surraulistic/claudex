package task

import (
	"context"
	"errors"
	"syscall"
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
	// flushThreadDeadline — предел на одно пробуждение треда: это не
	// ожидание, а цепочка вызовов app-server, и в три секунды она не
	// укладывается.
	flushThreadDeadline = 10 * time.Second
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

// Pushable — дожимается ли отчёт этому ведущему вообще. Тред и разговор —
// да: адрес и есть разговор. Панель — нет: за час она успевает сменить
// разговор, и единственная попытка — в момент done.
func Pushable(recs []journal.Record, id string) bool {
	k := stateOf(recs, id).kind
	return k == KindThread || k == KindSession
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
	// Безнадёжные сюда не попадают: адрес, молчавший неделю, не ответит и
	// сегодня, а место в пределе за проход он занимает.
	pending := LivePending(recs, time.Now())
	orphans := OrphanReports(recs, time.Now())
	if len(pending) == 0 && len(orphans) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, o.Budget)
	defer cancel()

	var out []Flushed
	for _, r := range orphans {
		if len(out) >= o.Max || ctx.Err() != nil {
			break
		}
		if !o.Skip[r.Task] {
			out = append(out, adoptOrphan(ctx, o, r))
		}
	}

	home := codex.Home()
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
		deadline := flushDeadline
		switch st.kind {
		case KindThread:
			// Живость треда здесь не требуется: очередь app-server переживает
			// закрытие и вычитывается при следующем пробуждении. Требовать её
			// значило бы вечно откладывать отчёт тому, кто просто закрыл
			// приложение, — ровно это и происходило с сотней отчётов.
			if st := codex.State(home, l.Target); st != codex.Live && st != codex.Known {
				continue
			}
			deadline = flushThreadDeadline
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
		state, why := StateOf(recs, l.Task).State, "отчёт был готов, но сразу не доставился ("+l.Reason+")"
		if l.Stage == StageSilence {
			state, why = StateLost, "исполнитель закончил и не отчитался; разбудить сразу не вышло ("+l.Reason+")"
		}
		d := Deliver(ctx, o.Client, o.Journal, l.Task, l.Target,
			flushText(l),
			DeliverOptions{Stage: l.Stage, Kind: st.kind, WantSession: l.Target,
				Compose: compose, HumanTold: true, FullReport: o.FullReport,
				Event: &Event{Task: l.Task, Stage: l.Stage, Executor: st.pane,
					State: state, Reason: why},
				Deadline: deadline, Poll: 200 * time.Millisecond})

		// Принятое в очередь, но не начатое — ещё не дошло: наблюдатель
		// отложит повтор, а не сочтёт доставленным.
		out = append(out, Flushed{
			Task: l.Task, Stage: l.Stage, Target: l.Target,
			Ok: d.Reached(), Reason: d.Reason,
		})
	}
	return out
}

// flushText — то, что уедет в разговор. Отчёт идёт целиком: ведущий его ещё не
// видел, и сокращать пересказом нечего.
func flushText(l Lost) string {
	if l.Stage == StageSilence {
		return "Поручение " + l.Task + ": исполнитель закончил и не отчитался; разбудить сразу не вышло (" +
			l.Reason + "), досылаем. Подробности: claudex task " + l.Task
	}
	head := "Отложенный отчёт по поручению " + l.Task +
		": доставить сразу не вышло (" + l.Reason + "), разговор открылся — досылаем."
	if l.Report == "" {
		return head
	}
	return head + "\n\n" + l.Report
}

// OrphanAfter — сколько ждать finished после отчёта, прежде чем проверять,
// жив ли ожидавший процесс. Живой пишет его за доли секунды.
const OrphanAfter = time.Minute

// waiterAlive подменяется в тестах. Переиспользованный pid выглядит живым —
// тогда отчёт остаётся ждать, как и прежде, а не уходит дважды.
var waiterAlive = func(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// OrphanReports — отчёты, чей ожидавший процесс умер, не записав finished
// (SIGKILL, падение машины). Report такие не доставляет — «наблюдатель ещё
// ждёт», — а ни дожим, ни молчание их не видели: notified нет, отчёт есть.
func OrphanReports(recs []journal.Record, now time.Time) []journal.Record {
	waiter := map[string]int{}
	finished := map[string]bool{}
	last := map[string]journal.Record{}
	var order []string
	for _, r := range recs {
		switch r.Event {
		case journal.Started:
			if r.WaiterPID > 0 {
				waiter[r.Task] = r.WaiterPID
			}
		case journal.Finished:
			finished[r.Task] = true
		case journal.Reported:
			if _, ok := last[r.Task]; !ok {
				order = append(order, r.Task)
			}
			last[r.Task] = r
		}
	}
	var out []journal.Record
	for _, id := range order {
		r, pid := last[id], waiter[id]
		if pid == 0 || finished[id] || now.Sub(r.Time) < OrphanAfter || now.Sub(r.Time) > AbandonAfter ||
			waiterAlive(pid) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// adoptOrphan записывает за пропавший процесс итог ожидания и отдаёт отчёт
// тем же путём, что поздний done.
func adoptOrphan(ctx context.Context, o FlushOptions, r journal.Record) Flushed {
	f := Flushed{Task: r.Task, Stage: StageReported}
	if err := o.Journal.Append(journal.Record{Task: r.Task, Event: journal.Finished, Outcome: Reported,
		Reason: "процесс ожидания пропал, не записав итог; отчёт отдаёт дожим"}); err != nil {
		f.Reason = err.Error()
		return f
	}
	recs, err := o.Journal.Read()
	if err != nil {
		f.Reason = err.Error()
		return f
	}
	st := stateOf(recs, r.Task)
	f.Target = st.target
	res, _ := deliverLate(ctx, st, r.Task, r.Pane, r.Outcome, r.Reason, ReportOptions{
		Client: o.Client, Journal: o.Journal, Compose: o.Compose, FullReport: o.FullReport,
		Deadline: flushThreadDeadline, Poll: 200 * time.Millisecond,
	}, ReportResult{Task: r.Task})
	f.Skipped = res.Skipped
	if res.Delivered != nil {
		f.Ok, f.Reason = res.Delivered.Reached(), res.Delivered.Reason
	}
	return f
}
