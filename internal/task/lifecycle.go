package task

import (
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

// Жизненный цикл поручения.
//
// Состояние не хранится отдельно: второй источник правды рассинхронизируется с
// журналом, а журнал дописывается одной строкой и переживает перезапуск.
// Поэтому состояние выводится из записей — ровно так же, как это делают
// ActiveFor и LostReports, только один раз и под одним именем.
//
// Заведено под будущего наблюдателя. Наблюдатель не пишет отчёт от себя: он
// фиксирует смену состояния в журнале и будит того, кто поручение затеял, а
// тот уже сам читает состояние и дайджест. Из-за этого состояния и понадобились
// именами: «ну что там» отвечается чтением, а не пересказом.
const (
	// StateCreated — текст поручения записан, отправка не подтверждена.
	// Возникает, когда исполнитель отказался принять задание.
	StateCreated = "created"
	// StateSent — поручение ушло исполнителю, отчёта ещё нет.
	StateSent = "sent"
	// StateWorking и StateProgress выводятся наблюдателем из живого состояния
	// исполнителя и промежуточных отметок. Пока наблюдателя нет, StateOf их не
	// возвращает: выдавать «занят» за «работает над этим» нельзя, занятость
	// исполнителя про наше поручение не доказывает ничего.
	StateWorking  = "working"
	StateProgress = "progress"
	// StateNeedsInput — задача сообщила, что упёрлась в вопрос.
	StateNeedsInput = "needs_input"
	// StateDone и StateFailed — задача отчиталась сама.
	StateDone   = "done"
	StateFailed = "failed"
	// StateUndelivered — отчёт есть, но до затеявшего он не дошёл. Лечится
	// дожимом, текст цел.
	StateUndelivered = "undelivered"
	// StateAbandoned — поручение висит без движения так долго, что ждать его
	// больше незачем. Отдельно от sent потому, что смешивать их дорого во всех
	// смыслах: список ожидающих распухает мёртвыми, и на вопрос «что сейчас»
	// отвечает сентябрём. Замерено: из 293 «ожидающих» 228 старше недели, и
	// они давали 85% веса `task list`.
	//
	// Не удаляется и не скрывается: `claudex task <id>` и `--all` показывают
	// его по-прежнему. Брошенное — не то же, что несуществующее.
	StateAbandoned = "abandoned"
	// StateLost — исполнитель закончил и промолчал. Отличается от sent тем,
	// что ждать больше нечего; устанавливается сборкой, не журналом.
	StateLost = "lost"
)

// Lifecycle — состояние поручения и то, чем оно доказано.
type Lifecycle struct {
	Task     string    `json:"task"`
	State    string    `json:"state"`
	Since    time.Time `json:"since,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Executor string    `json:"executor,omitempty"`
	Caller   string    `json:"caller,omitempty"`
	// CallerKind — чем адресуется затеявший: тред Codex, разговор Claude Code
	// или панель herdr. Наблюдателю он безразличен: будит он одинаково, а
	// разница живёт в доставке.
	CallerKind string `json:"caller_kind,omitempty"`
	// Delivery — чем затеявшего будили в последний раз и чем разбудят: событием
	// или отчётом целиком. Видно, чтобы «почему мне прилетела сводка» имело
	// ответ.
	Delivery string `json:"delivery,omitempty"`
}

// StateOf выводит состояние поручения из журнала.
//
// Чистая функция над записями: живое состояние исполнителя сюда не заглядывает
// намеренно. Занятость панели или разговора про наше поручение не доказывает
// ничего — этим занимается сборка, у которой есть право отказать с причиной.
// AbandonAfter — сколько поручение может висеть без движения, прежде чем его
// перестанут считать ожидающим. Трое суток: за это время успевает пройти
// выходные, и живая работа столько не молчит.
const AbandonAfter = 72 * time.Hour

func StateOf(recs []journal.Record, id string) Lifecycle {
	return StateAt(recs, id, time.Now())
}

// StateAt — то же, но на названный момент. Отдельно ради проверяемости:
// правило про давность иначе не проверить, не двигая системные часы.
func StateAt(recs []journal.Record, id string, now time.Time) Lifecycle {
	l := Lifecycle{Task: id, State: StateCreated}
	var started, reported, refused bool
	var delivered bool
	var sawNotify bool

	var last time.Time
	for _, r := range recs {
		if r.Task != id {
			continue
		}
		if r.Time.After(last) {
			last = r.Time
		}
		switch r.Event {
		case journal.Refused:
			refused = true
			l.Since, l.Reason = r.Time, r.Reason
		case journal.Started:
			started = true
			l.Since = r.Time
			l.Caller, l.CallerKind = r.Target, r.TargetKind
			l.Executor = executorName(r)
		case journal.Reported:
			reported = true
			l.Since = r.Time
			l.State = stateOfOutcome(r.Outcome)
			l.Reason = r.Reason
		case journal.Notified:
			sawNotify = true
			if r.Delivery != "" {
				l.Delivery = r.Delivery
			}
			if r.Stage == StageReported && r.Outcome == WokeUp {
				delivered = true
			}
		}
	}

	return settle(l, started, reported, refused, sawNotify, delivered, last, now)
}

// settle — правила, общие для обоих путей: одиночного разбора и прохода по
// всему журналу. Держать их в двух местах значит гарантированно разойтись.
func settle(l Lifecycle, started, reported, refused, notified, delivered bool,
	last, now time.Time) Lifecycle {

	if l.Delivery == "" && l.CallerKind != "" {
		l.Delivery = DeliveryFull
		if WakeCapable(l.CallerKind) {
			l.Delivery = DeliveryEvent
		}
	}
	switch {
	case reported:
		// Отчёт есть. Дошёл ли он — отдельный вопрос, и ответ на него важнее
		// исхода: неотданный отчёт выглядит для затеявшего как молчание.
		if notified && !delivered {
			l.State = StateUndelivered
		}
	case started:
		l.State = StateSent
	case refused:
		l.State = StateCreated
	}
	// Давность решает одинаково и для ждущих, и для недоставленных. Текст
	// недоставленного цел и дослать его можно когда угодно — но в ответе на
	// «что происходит сейчас» сентябрьскому отчёту места нет. Он никуда не
	// делся: `--all`, `claudex task <id>` и дожим видят его по-прежнему.
	if l.State == StateSent || l.State == StateUndelivered {
		if !last.IsZero() && now.Sub(last) > AbandonAfter {
			l.State = StateAbandoned
		}
	}
	return l
}

// stateOfOutcome — как сама задача назвала исход.
// OutcomeUnreported — исход, которым помечается собранное не самой задачей.
//
// Отдельным словом потому, что всё остальное читается как успех: любой
// незнакомый исход прежде становился done. Сборка с экрана панели записала так
// переписку человека с соседней сессией — и поручение стало «готово» с чужим
// текстом вместо отчёта.
const OutcomeUnreported = "без отчёта"

func stateOfOutcome(outcome string) string {
	switch first(outcome) {
	case "провал":
		return StateFailed
	case "заблокировано":
		return StateNeedsInput
	case "без":
		// «без отчёта»: работа кончилась, а чем — неизвестно.
		return StateLost
	default:
		return StateDone
	}
}

func first(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

// executorName — кому поручено: панель herdr либо разговор Claude Code.
func executorName(r journal.Record) string {
	if r.Pane != "" {
		return r.Pane
	}
	return r.PaneSession
}

// Open — поручения, по которым ещё чего-то ждут. Ровно те, о которых имеет
// смысл спрашивать «ну что там». Брошенные сюда не входят: список, где 85%
// строк — сентябрьские, на этот вопрос не отвечает.
func Open(recs []journal.Record) []Lifecycle { return open(recs, time.Now(), false) }

// OpenAll — включая брошенные.
func OpenAll(recs []journal.Record) []Lifecycle { return open(recs, time.Now(), true) }

// open проходит журнал один раз.
//
// Раньше здесь для каждого поручения вызывался StateAt, а тот пробегал все
// записи — на 456 поручениях и 1431 записи это шестьсот тысяч шагов, и дважды,
// потому что список просили и с брошенными, и без. Замерено: команда стала
// вчетверо медленнее. Порядок вывода сохраняется по первой записи о заведении.
func open(recs []journal.Record, now time.Time, withAbandoned bool) []Lifecycle {
	states := statesAt(recs, now)
	seen := map[string]bool{}
	var out []Lifecycle
	for _, r := range recs {
		if r.Event != journal.Started || seen[r.Task] {
			continue
		}
		seen[r.Task] = true
		l, ok := states[r.Task]
		if !ok {
			continue
		}
		switch l.State {
		case StateSent, StateUndelivered:
			out = append(out, l)
		case StateAbandoned:
			if withAbandoned {
				out = append(out, l)
			}
		}
	}
	return out
}

// statesAt — состояния всех поручений за один проход.
func statesAt(recs []journal.Record, now time.Time) map[string]Lifecycle {
	type acc struct {
		l                          Lifecycle
		last                       time.Time
		started, reported, refused bool
		notified, delivered        bool
	}
	all := map[string]*acc{}
	for _, r := range recs {
		a := all[r.Task]
		if a == nil {
			a = &acc{l: Lifecycle{Task: r.Task, State: StateCreated}}
			all[r.Task] = a
		}
		if r.Time.After(a.last) {
			a.last = r.Time
		}
		switch r.Event {
		case journal.Refused:
			a.refused = true
			a.l.Since, a.l.Reason = r.Time, r.Reason
		case journal.Started:
			a.started = true
			a.l.Since = r.Time
			a.l.Caller, a.l.CallerKind = r.Target, r.TargetKind
			a.l.Executor = executorName(r)
		case journal.Reported:
			a.reported = true
			a.l.Since = r.Time
			a.l.State = stateOfOutcome(r.Outcome)
			a.l.Reason = r.Reason
		case journal.Notified:
			a.notified = true
			if r.Delivery != "" {
				a.l.Delivery = r.Delivery
			}
			if r.Stage == StageReported && r.Outcome == WokeUp {
				a.delivered = true
			}
		}
	}
	out := make(map[string]Lifecycle, len(all))
	for id, a := range all {
		out[id] = settle(a.l, a.started, a.reported, a.refused, a.notified, a.delivered, a.last, now)
	}
	return out
}
