package task

import (
	"errors"
	"fmt"

	"github.com/surraulistic/claudex/internal/claudesess"
)

// Разрешение панели herdr в разговор Claude Code.
//
// Основным транспортом стал разговор. Панель адресует терминал, а не работу:
// она переживает смену агента, и «панель свободна» про разговор в ней не
// доказывает ничего — отсюда взялась защита executor_conversation_changed.
// Разговор же адресуется сам собой, и промахнуться им нельзя по устройству.
//
// Панель при этом не выбрасывается: она умеет то, чего сокет не умеет —
// запустить агента и показать экран. Поэтому поручение, разрешённое из панели,
// записывает в журнал и панель, и разговор: доставка идёт разговором, а сборка
// молчаливых отчётов по-прежнему может прочесть экран.
//
// Гадать здесь нельзя ни при каких условиях: промах уводит поручение в чужой
// разговор. Каждый неразрешённый случай — отказ с названной причиной.

// Причины отказа в разрешении панели. Машинно-читаемые: вызывающий разбирает
// их сравнением, а не поиском подстроки в тексте.
const (
	RefuseNotClaude        = "pane_target_not_claude"
	RefuseIdentityUnproven = "pane_identity_unproven"
	RefuseNoConversation   = "pane_conversation_absent"
	RefuseAmbiguous        = "pane_conversation_ambiguous"
	RefuseSelf             = "pane_is_current_conversation"
)

// PaneRefusal — почему панель не превратилась в разговор.
type PaneRefusal struct {
	Pane   string `json:"pane"`
	Cause  string `json:"cause"`
	Reason string `json:"reason"`
}

func (e *PaneRefusal) Error() string { return e.Reason }

// ConversationOfPane — разговор, который панель ведёт прямо сейчас.
//
// kind и session берутся из живого ответа herdr, а не из журнала: журнал
// помнит, каким разговор был, а адресовать надо тот, что там сейчас.
func ConversationOfPane(pane, kind, session string) (claudesess.Session, error) {
	refuse := func(cause, format string, a ...any) (claudesess.Session, error) {
		return claudesess.Session{}, &PaneRefusal{
			Pane: pane, Cause: cause, Reason: fmt.Sprintf(format, a...),
		}
	}
	if kind != "claude" {
		return refuse(RefuseNotClaude,
			"в панели %s работает %s, а не Claude Code: разговора у неё нет — "+
				"для прежнего транспорта добавьте --panel", pane, orUnknownKind(kind))
	}
	if session == "" {
		return refuse(RefuseIdentityUnproven,
			"herdr не сообщает разговор панели %s: доказать, кому адресовано поручение, нечем", pane)
	}

	s, err := claudesess.Resolve(claudesess.Home(), session)
	if err == nil {
		return s, nil
	}
	var amb *claudesess.Ambiguous
	switch {
	case errors.As(err, &amb):
		return refuse(RefuseAmbiguous,
			"панель %s указывает на несколько разговоров: %v", pane, amb.Error())
	case errors.Is(err, claudesess.ErrSelf):
		return refuse(RefuseSelf,
			"панель %s ведёт текущий разговор: поручать самому себе нечего", pane)
	default:
		return refuse(RefuseNoConversation,
			"разговор %s панели %s среди живых не значится: возможно, сессия закрыта — "+
				"для прежнего транспорта добавьте --panel", short(session), pane)
	}
}

func orUnknownKind(k string) string {
	if k == "" {
		return "неизвестный агент"
	}
	return k
}
