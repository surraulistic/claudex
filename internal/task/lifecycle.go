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
}

// StateOf выводит состояние поручения из журнала.
//
// Чистая функция над записями: живое состояние исполнителя сюда не заглядывает
// намеренно. Занятость панели или разговора про наше поручение не доказывает
// ничего — этим занимается сборка, у которой есть право отказать с причиной.
func StateOf(recs []journal.Record, id string) Lifecycle {
	l := Lifecycle{Task: id, State: StateCreated}
	var started, reported, refused bool
	var delivered bool
	var sawNotify bool

	for _, r := range recs {
		if r.Task != id {
			continue
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
			if r.Stage == StageReported && r.Outcome == WokeUp {
				delivered = true
			}
		}
	}

	switch {
	case reported:
		// Отчёт есть. Дошёл ли он — отдельный вопрос, и ответ на него важнее
		// исхода: неотданный отчёт выглядит для затеявшего как молчание.
		if sawNotify && !delivered {
			l.State = StateUndelivered
		}
	case started:
		l.State = StateSent
	case refused:
		l.State = StateCreated
	}
	return l
}

// stateOfOutcome — как сама задача назвала исход.
func stateOfOutcome(outcome string) string {
	switch first(outcome) {
	case "провал":
		return StateFailed
	case "заблокировано":
		return StateNeedsInput
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
// смысл спрашивать «ну что там».
func Open(recs []journal.Record) []Lifecycle {
	seen := map[string]bool{}
	var out []Lifecycle
	for _, r := range recs {
		if r.Event != journal.Started || seen[r.Task] {
			continue
		}
		seen[r.Task] = true
		if l := StateOf(recs, r.Task); l.State == StateSent || l.State == StateUndelivered {
			out = append(out, l)
		}
	}
	return out
}
