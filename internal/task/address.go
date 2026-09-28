package task

import (
	"fmt"
	"strings"

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
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Started && r.PaneSession != "" {
			return r.PaneSession, true
		}
	}
	return "", false
}
