package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/textual"
)

// Сбор отчёта за молчаливую задачу.
//
// Задача доводит работу до конца и не вызывает `claudex done`. В журнале
// остаются started и finished, отчёта нет, и ведущему не возвращается ничего —
// замерено на поручении 260031f8: панель дописала финал в 04:26:03, а
// управляющий разговор не узнал об этом вовсе.
//
// Отсюда сборка отчёта из живого экрана панели. Живого — потому что индекс cass
// отстаёт на часы, и подставить вместо финала вчерашний текст хуже, чем не
// подставить ничего.
//
// Освобождение панели доказательством не считается. Прежде чем что-то собирать,
// доказывается: закончил тот же разговор, что начинал; после нашего поручения
// этой панели ничего не поручали; на экране есть что взять. Не сошлось —
// записывается явная причина, а не догадка.

// Почему отчёт собрать нельзя.
const (
	CauseExecutorBusy    = "executor_busy"                 // панель ещё работает
	CauseExecutorChanged = "executor_conversation_changed" // в панели другой разговор
	CauseExecutorUnknown = "executor_binding_unproven"     // разговор при заведении не записан
	CauseSupersededTask  = "pane_took_later_task"          // после нашего поручения было ещё
	CauseNoFinalOutput   = "no_final_output"               // на экране нечего взять
	CauseNoHerdr         = "herdr_unavailable"
)

// freeForReport — состояния, в которых панель считается закончившей.
// blocked и unknown сюда не входят: первое ждёт человека, второе не значит
// ничего.
var freeForReport = map[string]bool{"idle": true, "done": true}

// ReconcileOptions — источники и пределы.
type ReconcileOptions struct {
	Journal *journal.Journal
	Client  *herdr.Client
	// Read — чтение живого экрана панели. Подменяется в тестах; настоящее
	// ходит в сокет herdr.
	Read func(pane string, lines int) (string, error)
	// Lines — сколько строк просить. Сверх видимой части панель заставляет
	// herdr восстанавливать прокрутку, и это стоит на порядок дороже.
	Lines int
	Max   int
	// MinAge — сколько ждать, прежде чем считать молчание окончательным.
	// Задача может вызвать done через минуту после того, как панель показала
	// финал, и перехватывать её незачем.
	MinAge  time.Duration
	Now     time.Time
	Compose func(summary, task string, from, to time.Time, pane string) string
}

type Reconciled struct {
	Task      string `json:"task"`
	Pane      string `json:"pane,omitempty"`
	Synthetic bool   `json:"synthetic,omitempty"`
	Delivered bool   `json:"delivered,omitempty"`
	Cause     string `json:"cause,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

const (
	defaultReconcileMax   = 3
	defaultReconcileAge   = 5 * time.Minute
	defaultReconcileLines = herdr.CheapLines
)

// Reconcile собирает отчёты за задачи, которые закончили молча.
func Reconcile(ctx context.Context, o ReconcileOptions) []Reconciled {
	if o.Journal == nil {
		return nil
	}
	if o.Max <= 0 {
		o.Max = defaultReconcileMax
	}
	if o.MinAge <= 0 {
		o.MinAge = defaultReconcileAge
	}
	if o.Lines <= 0 {
		o.Lines = defaultReconcileLines
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	recs, err := o.Journal.Read()
	if err != nil {
		return nil
	}

	var out []Reconciled
	for _, a := range silent(recs, o.Now, o.MinAge) {
		if len(out) >= o.Max || ctx.Err() != nil {
			break
		}
		r := reconcileOne(ctx, recs, a, o)
		out = append(out, r)
	}
	return out
}

// silent — поручения, у которых есть начало и нет отчёта, и с начала прошло
// достаточно, чтобы молчание считать окончательным.
func silent(recs []journal.Record, now time.Time, minAge time.Duration) []Assignment {
	var out []Assignment
	for _, a := range ActiveFor(recs, "") {
		if a.Pane == "" || now.Sub(a.Started) < minAge {
			continue
		}
		out = append(out, a)
	}
	return out
}

func reconcileOne(ctx context.Context, recs []journal.Record, a Assignment, o ReconcileOptions) Reconciled {
	r := Reconciled{Task: a.Task, Pane: a.Pane}

	bound := paneSessionOf(recs, a.Task)
	if bound == "" {
		return refuse(r, CauseExecutorUnknown,
			"разговор панели при заведении не записан: доказать, что закончил он, нечем")
	}
	if later := laterTaskFor(recs, a); later != "" {
		return refuse(r, CauseSupersededTask,
			fmt.Sprintf("после этого поручения панели дали ещё одно (%s): экран показывает не наш финал", later))
	}
	if o.Client == nil {
		return refuse(r, CauseNoHerdr, "herdr недоступен: живой экран прочитать нечем")
	}
	ag, err := o.Client.Get(a.Pane)
	if err != nil {
		return refuse(r, CauseNoHerdr, "панель не отвечает: "+err.Error())
	}
	if !freeForReport[ag.Status] {
		return refuse(r, CauseExecutorBusy,
			fmt.Sprintf("панель в состоянии %q: работа не закончена", ag.Status))
	}
	if now := ag.Session.Value; now != bound {
		return refuse(r, CauseExecutorChanged,
			fmt.Sprintf("в панели теперь разговор %s, а поручение получал %s",
				short(now), short(bound)))
	}

	text := finalOf(o, a.Pane)
	if text == "" {
		return refuse(r, CauseNoFinalOutput,
			"на живом экране панели нечего взять: финала нет")
	}

	// Отчёт кладётся в журнал так же, как обычный, и помечается собранным:
	// доверие к нему другое, и это должно быть видно.
	o.Journal.Append(journal.Record{
		Task: a.Task, Event: journal.Reported, Pane: a.Pane,
		Outcome: "готово", Reason: text, Synthetic: true,
		Correction: "отчёт собран ClauDex с живого экрана панели: задача не вызвала claudex done",
	})
	r.Synthetic = true

	// Дальше — общий путь: та же проверка адреса, тот же inbox, та же
	// дедупликация. Собранный отчёт не получает поблажек.
	st := stateOf(recs, a.Task)
	w := WakeOf(st.kind, st.target, st.session)
	if w.Target == "" {
		r.Cause, r.Reason = CauseUnconfirmedBinding, "ведущий не назначался"
		return r
	}
	var compose func(string) string
	if o.Compose != nil {
		id, from, pane := a.Task, st.started, st.pane
		compose = func(summary string) string {
			return o.Compose(summary, id, from, time.Now(), pane)
		}
	}
	d := Deliver(ctx, o.Client, o.Journal, a.Task, w.Target,
		fmt.Sprintf("Поручение %s закончено, но отчёта задача не дала — ClauDex собрал его с живого экрана панели %s:\n\n%s",
			a.Task, a.Pane, text),
		DeliverOptions{Stage: StageReported, Kind: w.Kind, WantSession: w.Session,
			Compose: compose, Deadline: 15 * time.Second, Poll: time.Second})
	r.Delivered, r.Cause, r.Reason = d.OK, d.Cause, d.Reason
	return r
}

// refuse записывает отказ так, чтобы он был виден в канале вытягивания, а не
// потерялся молча.
func refuse(r Reconciled, cause, reason string) Reconciled {
	r.Cause, r.Reason = cause, reason
	return r
}

// paneSessionOf — разговор панели-исполнителя, записанный при заведении.
func paneSessionOf(recs []journal.Record, id string) string {
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Started {
			return r.PaneSession
		}
	}
	return ""
}

// laterTaskFor — поручение, выданное той же панели позже нашего. Если оно есть,
// экран показывает чужой финал.
func laterTaskFor(recs []journal.Record, a Assignment) string {
	for _, r := range recs {
		if r.Event == journal.Started && r.Pane == a.Pane &&
			r.Task != a.Task && r.Time.After(a.Started) {
			return r.Task
		}
	}
	return ""
}

// finalOf — последний осмысленный кусок живого экрана.
func finalOf(o ReconcileOptions, pane string) string {
	read := o.Read
	if read == nil {
		read = func(p string, n int) (string, error) {
			return o.Client.Read(p, "recent", n)
		}
	}
	raw, err := read(pane, o.Lines)
	if err != nil {
		return ""
	}
	lines := textual.CleanTail(raw, o.Lines)
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if len([]rune(out)) < 40 {
		// Пустая рамка приглашения — это не финал.
		return ""
	}
	return out
}
