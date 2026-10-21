package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
	"github.com/surraulistic/claudex/internal/journal"
)

// Распознавание первого довода `claudex send`.
//
// Флагов в обиходе быть не должно: человек пишет `claudex send 81fad209 "…"`
// и вправе рассчитывать, что его поймут. Понять можно три разные вещи, и
// путать их нельзя — они ведут в разные места.
//
// Порядок разбора по убыванию точности:
//
//  1. идентификатор разговора — отправка в него;
//  2. идентификатор поручения — продолжение уже начатой работы;
//  3. всё остальное — имя или метка панели herdr, привычный способ назвать
//     работника глазами.
//
// Первые два совпадают по форме: идентификатор поручения — восемь
// шестнадцатеричных, и ровно так же выглядит начало идентификатора разговора.
// Когда подходят оба, угадывать нельзя: одно отправит сообщение работнику,
// другое продолжит чужую работу. Такой случай — отказ с обоими толкованиями.

const (
	AddrSession = "session"
	AddrTask    = "task"
	AddrPane    = "pane"
)

// Addressee — как понят довод.
type Addressee struct {
	Kind    string             `json:"kind"`
	Raw     string             `json:"raw"`
	Session claudesess.Session `json:"session,omitempty"`
	Task    string             `json:"task,omitempty"`
}

// AmbiguousAddress — довод подходит и разговору, и поручению.
type AmbiguousAddress struct {
	Raw     string
	Session string
	Task    string
}

func (e *AmbiguousAddress) Error() string {
	return fmt.Sprintf(
		"%q подходит и разговору %s, и поручению %s — угадывать нельзя.\n"+
			"  отправить в разговор:  claudex send --session %s \"…\"\n"+
			"  продолжить поручение:  claudex task %s  (и затем send его исполнителю)",
		e.Raw, short(e.Session), e.Task, e.Session, e.Task)
}

// Classify разбирает довод. home — каталог реестра разговоров; отдельным
// доводом ради тестов.
func Classify(raw, home string, recs []journal.Record) (Addressee, error) {
	raw = strings.TrimSpace(raw)
	a := Addressee{Raw: raw}
	if raw == "" {
		return a, fmt.Errorf("адресат не назван")
	}

	// Разговор ищется только по идентификатору. Имя сюда не пускается
	// намеренно: имена разговора и панели обычно совпадают, и тогда правило
	// «сначала разговор» тихо забрало бы у панели её привычную работу.
	var sess claudesess.Session
	var haveSess bool
	if looksLikeID(raw) {
		if s, err := claudesess.Lookup(home, raw); err == nil {
			sess, haveSess = s, true
		}
	}
	haveTask := IsTaskID(raw) && knowsTask(recs, raw)

	switch {
	case haveSess && haveTask:
		return a, &AmbiguousAddress{Raw: raw, Session: sess.ID, Task: raw}
	case haveSess:
		a.Kind, a.Session = AddrSession, sess
	case haveTask:
		a.Kind, a.Task = AddrTask, raw
	default:
		a.Kind = AddrPane
	}
	return a, nil
}

// looksLikeID — похоже ли на идентификатор разговора: восемь и больше
// шестнадцатеричных, возможно с дефисами полного вида.
func looksLikeID(s string) bool {
	n := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
			n++
		case c == '-':
		default:
			return false
		}
	}
	return n >= 8
}

func knowsTask(recs []journal.Record, id string) bool {
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Started {
			return true
		}
	}
	return false
}

// ExecutorOfTask — кому поручение было отдано. Нужен продолжению: дописывать
// работу надо тому же исполнителю, а не заводить её заново.
func ExecutorOfTask(recs []journal.Record, id string) (string, bool) {
	var out string
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Started {
			out = r.PaneSession
		}
	}
	return out, out != ""
}

// HeadlessExecutor returns the persisted identity and launch directory of a
// native Claude Code background session assigned to a task.
func HeadlessExecutor(recs []journal.Record, id string) (sessionID, cwd string, ok bool) {
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Started && r.Headless {
			sessionID, cwd = r.PaneSession, r.CWD
		}
	}
	return sessionID, cwd, sessionID != ""
}

// OpenFor — открытые поручения этого исполнителя, от свежих к старым.
//
// Нужно `send`: отправить работнику, у которого работа уже идёт, — это чаще
// всего продолжение, а не второе поручение. Прежде каждая отправка заводила
// отдельное, и на живом журнале у одного исполнителя их накопилось 62, ни
// одно не закрыто.
func OpenFor(recs []journal.Record, who string) []Assignment {
	a := ActiveFor(recs, who)
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}
	return a
}

// ContinueTarget — какое открытое поручение исполнителя продолжает отправка от
// sender. Пустой id без ошибки — заводить новое, why говорит почему.
//
// Продолжение возвращает результат тому, кто затеял исходное поручение, —
// поэтому продолжать можно только своё: чужое увело бы ответ прежнему ведущему.
// Брошенное не продолжается: его уже никто не ждёт.
func ContinueTarget(recs []journal.Record, executor string, sender Wake, now time.Time) (id, why string, err error) {
	open := OpenFor(recs, executor)
	if len(open) == 0 {
		return "", "", nil
	}
	states := statesAt(recs, now)
	var live, mine []Assignment
	for _, a := range open {
		if states[a.Task].State == StateAbandoned {
			continue
		}
		live = append(live, a)
		if sameCaller(a, sender) {
			mine = append(mine, a)
		}
	}
	switch {
	case len(live) == 0:
		return "", fmt.Sprintf("открытые поручения %s брошены", short(executor)), nil
	case sender.Empty():
		return "", "", &SenderUnknown{Who: executor, Open: live}
	case len(mine) == 1:
		return mine[0].Task, "", nil
	case len(mine) > 1:
		return "", "", &TooManyOpen{Who: executor, Open: mine}
	}
	return "", fmt.Sprintf("открытое у %s поручение %s затеял другой ведущий (%s)",
		short(executor), live[0].Task, short(live[0].Target)), nil
}

// MayContinue — можно ли sender продолжить названное поручение.
//
// Явный идентификатор — явное намерение, но результат продолжения уходит
// тому, кто поручение затеял. Поэтому известный отправитель продолжает только
// своё. Отправитель не определён (человек из терминала, скрипт) — продолжение
// разрешено: он назвал поручение сам, а ответ вернётся прежнему ведущему.
func MayContinue(recs []journal.Record, id string, sender Wake) error {
	if sender.Empty() {
		return nil
	}
	var a Assignment
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Started {
			a = Assignment{Task: id, Target: r.Target, Kind: r.TargetKind, Session: r.TargetSession}
		}
	}
	if a.Target == "" || sameCaller(a, sender) {
		return nil
	}
	return &ForeignTask{Task: id, Caller: a.Target}
}

// ForeignTask — поручение затеял другой ведущий: продолжение увело бы ответ
// ему.
type ForeignTask struct {
	Task, Caller string
}

func (e *ForeignTask) Error() string {
	return fmt.Sprintf("поручение %s затеял другой ведущий (%s): результат продолжения вернётся ему, а не вам.\n"+
		"  своё поручение тому же исполнителю: claudex send <разговор> --new \"…\"",
		e.Task, short(e.Caller))
}

// sameCaller — тот ли это ведущий. У панели адрес — место, а не разговор,
// поэтому нужен ещё и тот же разговор в ней.
func sameCaller(a Assignment, w Wake) bool {
	kind := orElse(a.Kind, KindPane)
	if w.Empty() || kind != w.Kind || a.Target != w.Target {
		return false
	}
	return kind != KindPane || a.Session != "" && a.Session == w.Session
}

// SenderUnknown — отправитель не определён, а у исполнителя открыта работа:
// чья она, не угадать, и продолжение могло бы увести ответ чужому ведущему.
type SenderUnknown struct {
	Who  string
	Open []Assignment
}

func (e *SenderUnknown) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "отправитель не определён (нет ни треда Codex, ни разговора Claude Code, ни панели herdr), "+
		"а у %s открытых поручений %d — чьё это продолжение, не угадать:", short(e.Who), len(e.Open))
	listOpen(&b, e.Open)
	return b.String()
}

// TooManyOpen — у работника несколько незакрытых поручений, и какое из них
// продолжают, знает только человек.
type TooManyOpen struct {
	Who  string
	Open []Assignment
}

func (e *TooManyOpen) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "у %s незакрытых поручений %d — какое продолжаем, не угадать:",
		short(e.Who), len(e.Open))
	listOpen(&b, e.Open)
	return b.String()
}

func listOpen(b *strings.Builder, open []Assignment) {
	for i, a := range open {
		if i == 5 {
			fmt.Fprintf(b, "\n  … и ещё %d", len(open)-5)
			break
		}
		fmt.Fprintf(b, "\n  %s  %s", a.Task, a.Started.Format("02.01 15:04"))
	}
	b.WriteString("\n  продолжить названное: claudex send <id поручения> \"…\"" +
		"\n  завести отдельное:    claudex send <разговор> --new \"…\"")
}
