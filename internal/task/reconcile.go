package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
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
	Task string `json:"task"`
	Pane string `json:"pane,omitempty"`
	// Session — разговор-исполнитель, когда панели у поручения нет. Без него
	// вывод называет такое поручение пустыми скобками.
	Session   string `json:"session,omitempty"`
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

	// Предел делится не поровну. Панельное поручение стоит чтения экрана через
	// herdr, поручение в разговор — одного файла в реестре, и делить с панелями
	// общий предел ему незачем. С общим оно и не рассматривалось никогда:
	// замерено на живом журнале — 169 активных поручений при пределе 3, и
	// свежее сессионное стояло 168-м.
	var conv, panes []Assignment
	var hopeless int
	for _, a := range silent(recs, o.Now, o.MinAge) {
		// Безнадёжные не занимают предел. У поручений, заведённых до записи
		// разговора панели, привязку доказать нечем и уже не будет чем: они
		// отказывают всегда. Пока они стояли в голове очереди, до остальных
		// дело не доходило вовсе — за 424 поручения сборка не собрала ни
		// одного отчёта.
		if a.Pane != "" && paneSessionOf(recs, a.Task) == "" {
			hopeless++
			continue
		}
		if a.Pane == "" {
			conv = append(conv, a)
		} else {
			panes = append(panes, a)
		}
	}
	// Свежие первыми: человек ждёт вчерашнее поручение, а не сентябрьское.
	reverse(panes)
	reverse(conv)

	var out []Reconciled
	for _, a := range conv {
		if ctx.Err() != nil {
			break
		}
		out = append(out, reconcileOne(ctx, recs, a, o))
	}
	examined := 0
	for _, a := range panes {
		if examined >= o.Max || ctx.Err() != nil {
			break
		}
		out = append(out, reconcileOne(ctx, recs, a, o))
		examined++
	}
	// Про нерассмотренное говорится вслух. Прежде очередь обрывалась молча, и
	// отсутствие поручения в выдаче было неотличимо от «с ним всё в порядке».
	// Безнадёжные названы разом. Молчать о них нельзя — это те самые
	// поручения, чей отчёт уже не будет собран никогда; но и место в очереди
	// им отдавать незачем: они отказывают всегда и держат её голову.
	if hopeless > 0 {
		out = append(out, Reconciled{
			Cause: CauseExecutorUnknown,
			Reason: fmt.Sprintf("%d поручений без записанного разговора-исполнителя: "+
				"привязку доказать нечем, отчёт по ним собран не будет", hopeless),
		})
	}
	if left := len(panes) - examined; left > 0 {
		out = append(out, Reconciled{
			Cause: CauseNotExamined,
			Reason: fmt.Sprintf("ещё %d панельных поручений не рассмотрено за этот вызов "+
				"(предел %d); повторите вызов или поднимите предел", left, o.Max),
		})
	}
	return out
}

// CauseNotExamined — до поручения не дошла очередь за этот вызов.
const CauseNotExamined = "not_examined"

// silent — поручения, у которых есть начало и нет отчёта, и с начала прошло
// достаточно, чтобы молчание считать окончательным.
func silent(recs []journal.Record, now time.Time, minAge time.Duration) []Assignment {
	// Разбуженных не трогаем. Отчёта здесь не появляется — появляется
	// пробуждение, и без этой проверки поручение осталось бы активным
	// навсегда и будило бы затеявшего при каждом проходе.
	woken := map[string]bool{}
	for _, r := range recs {
		if r.Event == journal.Notified && r.Stage == StageReported && r.Outcome == WokeUp {
			woken[r.Task] = true
		}
	}
	var out []Assignment
	for _, a := range ActiveFor(recs, "") {
		if woken[a.Task] {
			continue
		}
		if now.Sub(a.Started) < minAge {
			continue
		}
		out = append(out, a)
	}
	return out
}

func reconcileOne(ctx context.Context, recs []journal.Record, a Assignment, o ReconcileOptions) Reconciled {
	r := Reconciled{Task: a.Task, Pane: a.Pane}

	if a.Pane == "" {
		return reconcileConversation(recs, a, r)
	}

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

	// Отметка хука точнее экрана: платформа отдаёт текст реплики как есть, без
	// рамок и без прокрутки. Экран остаётся запасным путём — для панелей, где
	// хук не установлен.
	text := lastSaid(recs, a.Task)
	if text == "" {
		text = finalOf(o, a.Pane)
	}
	if text == "" {
		return refuse(r, CauseNoFinalOutput,
			"на живом экране панели нечего взять: финала нет")
	}

	// Отчёт отсюда не пишется.
	//
	// Экран панели — свидетельство чего-то, но не доказательство, что задача
	// закончила именно этим. Замерено за всё время работы: собранных с экрана
	// отчётов два, и оба мусор, помеченный как «готово», — обрывок таблицы и
	// переписка человека с соседней сессией. Ноль верных из двух.
	//
	// Поэтому экран уходит доказательством в событие, а отчётом не
	// становится: затеявший узнаёт, что работа кончилась и что видно на
	// экране, и решает сам. Настоящий текст даёт хук Stop — тогда, когда может
	// отнести его к одному поручению.

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
		fmt.Sprintf("Поручение %s: панель %s освободилась, отчёта задача не дала. "+
			"На экране было (это не отчёт, а то, что там видно):\n\n%s",
			a.Task, a.Pane, text),
		DeliverOptions{Stage: StageReported, Kind: w.Kind, WantSession: w.Session,
			Compose: compose,
			Event: &Event{Task: a.Task, Stage: StageReported, Executor: a.Pane,
				State:  StateLost,
				Reason: "закончила и не отчиталась; на экране: " + oneLineCut(text, EventChars)},
			Deadline: 15 * time.Second, Poll: time.Second})
	r.Delivered, r.Cause, r.Reason = d.OK, d.Cause, d.Reason
	if d.OK && r.Cause == "" {
		// Отчёта не появилось — появилось пробуждение. Называется честно.
		r.Cause, r.Reason = CauseNoScreen, "затеявший разбужен наблюдением, отчёта задачи нет"
	}
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

// CauseNoScreen — разговор закончил и промолчал, а взять текст неоткуда.
//
// У поручения в разговор Claude Code панели нет, и живого экрана тоже: сочинять
// за него отчёт нельзя. Но и пропадать поручение не должно — прежде silent()
// отсеивал всё беспанельное, и такое молчание не попадало ни в сборку, ни в
// канал вытягивания. Теперь оно видно с названной причиной.
const CauseNoScreen = "conversation_has_no_screen"

// reconcileConversation называет, чем кончилось молчание разговора.
//
// Собрать отчёт отсюда невозможно, и выдумывать его запрещено ровно так же, как
// на пустом экране панели. Зато реестр отличает «ещё работает» от «закончил и
// промолчал», а это и есть то, чего ведущему не хватало.
func reconcileConversation(recs []journal.Record, a Assignment, r Reconciled) Reconciled {
	id := paneSessionOf(recs, a.Task)
	if id == "" {
		return refuse(r, CauseExecutorUnknown,
			"разговор-исполнитель при заведении не записан: проверять нечего")
	}
	s, err := claudesess.Lookup(claudesess.Home(), id)
	if err != nil {
		return refuse(r, CauseSessionGone,
			fmt.Sprintf("разговор %s больше не жив: отчёта по поручению не будет", short(id)))
	}
	if s.Busy() {
		r.Session = s.ID
		return refuse(r, CauseExecutorBusy,
			fmt.Sprintf("разговор %s занят: работа не закончена", s.Short()))
	}
	r.Session = s.ID
	return refuse(r, CauseNoScreen,
		fmt.Sprintf("разговор %s свободен, а отчёта не дал; экрана у разговора нет, "+
			"взять текст неоткуда — попросите его вызвать claudex done %s", s.Short(), a.Task))
}

// lastSaid — последняя реплика исполнителя, записанная хуком Stop.
func lastSaid(recs []journal.Record, id string) string {
	var out string
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Idle && strings.TrimSpace(r.Reason) != "" {
			out = strings.TrimSpace(r.Reason)
		}
	}
	return out
}

func reverse(a []Assignment) {
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}
}
