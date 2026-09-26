package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

// Назначение — это запись о заведении поручения: в ней уже есть панель, адрес
// возврата и время. Отдельного файла состояния намеренно нет — второй источник
// правды рассинхронизируется, а журнал дописывается одной строкой и переживает
// перезапуск.
//
// Поручение активно, пока по нему не пришёл отчёт. Событие finished закрытием
// не считается: у отправки без ожидания оно пишется сразу, в ту же секунду.
type Assignment struct {
	Task    string    `json:"task"`
	Pane    string    `json:"pane,omitempty"`
	Target  string    `json:"target,omitempty"`
	Kind    string    `json:"kind,omitempty"`
	Session string    `json:"session,omitempty"`
	Started time.Time `json:"started"`
}

// IsTaskID — похоже ли это на идентификатор поручения: восемь шестнадцатеричных.
//
// Нужно, чтобы отличить назначенный id от текста отчёта, поставленного первым
// доводом. В журнале такие записи есть: три отчёта уехали в поле task целиком.
func IsTaskID(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ActiveFor — поручения этой панели, по которым отчёта ещё не было.
// Пустая панель значит «по всем панелям».
// executedBy — этому ли исполнителю заведено поручение.
//
// Исполнитель бывает двух видов. У панельного поручения это панель herdr, у
// поручения в разговор Claude Code панели нет вовсе, и исполнителем записан
// разговор. Сопоставлять только по панели значит отказывать разговору в
// собственном отчёте: живая проверка на поручении e7c83a2b дала ровно это —
// «заведено для другой панели», хотя заведено оно было не для панели.
func executedBy(r journal.Record, who string) bool {
	if who == "" {
		return true
	}
	return r.Pane == who || r.PaneSession == who
}

func ActiveFor(recs []journal.Record, pane string) []Assignment {
	reported := map[string]bool{}
	for _, r := range recs {
		if r.Event == journal.Reported {
			reported[r.Task] = true
		}
	}
	seen := map[string]bool{}
	var out []Assignment
	for _, r := range recs {
		if r.Event != journal.Started || seen[r.Task] {
			continue
		}
		if !executedBy(r, pane) {
			continue
		}
		seen[r.Task] = true
		if reported[r.Task] {
			continue
		}
		out = append(out, Assignment{
			Task: r.Task, Pane: r.Pane, Target: r.Target,
			Kind: r.TargetKind, Session: r.TargetSession, Started: r.Time,
		})
	}
	return out
}

// Почему названный идентификатор не приняли как есть.
const (
	ClaimNone    = "id не назван"
	ClaimNotID   = "первым доводом пришёл текст отчёта, а не идентификатор"
	ClaimUnknown = "такого поручения в журнале нет"
	ClaimClosed  = "по этому поручению отчёт уже был"
	ClaimForeign = "это поручение заведено другому исполнителю"
)

// Resolution — под каким идентификатором писать отчёт и почему он не тот,
// который назвали.
type Resolution struct {
	Task      string `json:"task"`
	Claimed   string `json:"claimed,omitempty"`
	Corrected bool   `json:"corrected,omitempty"`
	Cause     string `json:"cause,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Resolve решает, какому поручению принадлежит отчёт.
//
// Доверять названному идентификатору нельзя: сессия в панели живёт часами и
// переживает компакт, после которого помнит первый увиденный id, а не текущий.
// Замерено на живом журнале — панель wE:p13 семнадцать раз отчиталась в
// поручение, закрытое ещё утром, и настоящее поручение осталось без отчёта.
//
// Поэтому идентификатор принимается, только если он и правда активен у этой
// панели. Иначе — исправление на единственное активное поручение, с пометкой в
// журнале. Исправление выбрано вместо отказа намеренно: отказ оставил бы
// ведущего неразбуженным, то есть ровно тот сбой, ради которого всё делается.
//
// Угадывать, когда активных ноль или больше одной, нельзя: ложный отчёт хуже
// отсутствующего, потому что он выглядит как настоящий.
func Resolve(recs []journal.Record, pane, claimed string) (Resolution, error) {
	claimed = strings.TrimSpace(claimed)
	active := ActiveFor(recs, pane)

	cause := claimFault(recs, pane, claimed)
	if cause == "" {
		return Resolution{Task: claimed, Claimed: claimed}, nil
	}
	if len(active) == 1 {
		a := active[0]
		r := Resolution{Task: a.Task, Claimed: claimed, Corrected: true, Cause: cause}
		r.Note = fmt.Sprintf("отчёт записан под %s: %s (названо: %s)",
			a.Task, cause, orNamed(claimed))
		return r, nil
	}
	if len(active) == 0 {
		// Точный идентификатор собственного поручения — не догадка, даже если
		// отчёт по нему уже был: повторный done служит повтором доставки,
		// которая никого не достигла. Исправлять тут нечего и не на что.
		if cause == ClaimClosed && startedHere(recs, pane, claimed) {
			return Resolution{Task: claimed, Claimed: claimed}, nil
		}
		return Resolution{}, fmt.Errorf(
			"%s, а активных поручений у %s нет — отчёт записывать не под что; посмотреть: claudex tasks",
			cause, orPane(pane))
	}
	return Resolution{}, fmt.Errorf(
		"%s, а активных поручений у %s несколько (%s) — угадывать нельзя, назовите идентификатор явно",
		cause, orPane(pane), strings.Join(ids(active), ", "))
}

// claimFault — что не так с названным идентификатором. Пусто значит «всё так».
func claimFault(recs []journal.Record, pane, claimed string) string {
	switch {
	case claimed == "":
		return ClaimNone
	case !IsTaskID(claimed):
		return ClaimNotID
	}
	for _, a := range ActiveFor(recs, pane) {
		if a.Task == claimed {
			return ""
		}
	}
	// Поручение известно, но этому исполнителю уже не принадлежит: либо отчёт
	// по нему был, либо заводили его другому. Исполнителем бывает и панель
	// herdr, и разговор Claude Code — у поручения в разговор панели нет вовсе,
	// и сверка по одной панели объявляла такое поручение чужим.
	for _, r := range recs {
		if r.Task != claimed {
			continue
		}
		if r.Event == journal.Reported {
			return ClaimClosed
		}
		if r.Event == journal.Started && pane != "" && !executedBy(r, pane) {
			return ClaimForeign
		}
	}
	return ClaimUnknown
}

// startedHere — заводилось ли это поручение именно этому исполнителю.
func startedHere(recs []journal.Record, pane, task string) bool {
	for _, r := range recs {
		if r.Event == journal.Started && r.Task == task && executedBy(r, pane) {
			return true
		}
	}
	return false
}

func ids(a []Assignment) []string {
	out := make([]string, 0, len(a))
	for _, x := range a {
		out = append(out, x.Task)
	}
	return out
}

func orNamed(s string) string {
	if s == "" {
		return "ничего"
	}
	if len([]rune(s)) > 40 {
		return string([]rune(s)[:40]) + "…"
	}
	return s
}

// orPane называет исполнителя в отказе. Им бывает и панель herdr, и разговор
// Claude Code: у поручения в разговор панели нет вовсе, и называть его панелью
// значит сбивать с толку ровно там, где человек разбирается, что пошло не так.
func orPane(p string) string {
	switch {
	case p == "":
		return "(исполнитель не определён)"
	case strings.Count(p, "-") == 4:
		return "разговора " + p[:8]
	default:
		return "панели " + p
	}
}
