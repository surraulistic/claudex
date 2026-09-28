package task

import (
	"fmt"
	"strings"
)

// Пробуждение событием вместо отчёта в диалог.
//
// Прежде ClauDex клал в разговор затеявшего весь отчёт целиком — «Отложенный
// отчёт по поручению…», «Сводка ClauDex, не ответ Claude». Человек видел
// служебную переписку инструмента там, где ждал ответа собеседника, а
// координатор получал готовый пересказ и повторял его своими словами.
//
// Правильнее разбудить минимальным событием: случилось вот что, читай сам.
// Тогда в диалоге остаётся только то, что сказал координатор, а подробности он
// берёт из `claudex task` и `claudex task log` — то есть из первоисточника, а
// не из пересказа.
//
// Событие имеет смысл лишь тому, кто умеет его отработать: «иди прочитай»
// выполнимо для треда Codex и разговора Claude Code — они запускают команды.
// Уведомление herdr читает человек, и отправить ему «прочитай сам» значит
// переложить работу инструмента на того, кто за ней не следит. Поэтому панель
// по-прежнему получает отчёт целиком, и это не недоделка, а разные адресаты.

// Способы доставки.
const (
	DeliveryEvent = "event_wake"
	DeliveryFull  = "full_report"
)

// WakeCapable — сможет ли адресат прочитать поручение сам.
func WakeCapable(kind string) bool {
	return kind == KindThread || kind == KindSession
}

// Event — минимальное пробуждение.
type Event struct {
	Task string `json:"task"`
	// State — состояние поручения из журнала: done, failed, needs_input,
	// undelivered. Называется прямо, чтобы координатор решил, нужно ли ему
	// вообще читать подробности.
	State string `json:"state"`
	Stage string `json:"stage,omitempty"`
	// Reason — одна строка, зачем разбудили. Не отчёт и не его начало:
	// обрезанный отчёт хуже отсутствующего, потому что выглядит полным.
	Reason string `json:"reason,omitempty"`
	// Executor — кто работал. Нужен, чтобы координатор узнал поручение, не
	// заглядывая в журнал.
	Executor string `json:"executor,omitempty"`
}

// EventChars — предел на строку причины. Событие обязано оставаться событием:
// всё, что длиннее, — уже пересказ, а его читают из первоисточника.
const EventChars = 120

// Text — то, что уедет в разговор.
func (e Event) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "claudex: поручение %s — %s", e.Task, orState(e.State))
	if e.Executor != "" {
		fmt.Fprintf(&b, " (исполнитель %s)", e.Executor)
	}
	if r := oneLineCut(e.Reason, EventChars); r != "" {
		fmt.Fprintf(&b, "\n%s", r)
	}
	fmt.Fprintf(&b, "\n\nЭто событие, а не отчёт. Подробности читай сам:\n"+
		"  claudex task %s\n  claudex task log %s", e.Task, e.Task)
	return b.String()
}

func orState(s string) string {
	switch s {
	case StateDone:
		return "готово"
	case StateFailed:
		return "провал"
	case StateNeedsInput:
		return "нужен ответ"
	case StateUndelivered:
		return "отчёт есть, доставить не удавалось"
	case StateLost:
		return "закончило молча"
	case "":
		return "состояние изменилось"
	default:
		return s
	}
}

func oneLineCut(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " · "))
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// payload выбирает, что именно отправить, и называет выбранный способ.
//
// Полный отчёт остаётся для тех, кто прочитать сам не может, и для случаев,
// когда его попросили явно. Способ возвращается наружу, чтобы попасть в журнал:
// иначе по записи не отличить «разбудили событием» от «прислали всё целиком»,
// а разбираться в этом потом придётся.
func payload(text string, o DeliverOptions, kind string) (string, string) {
	if o.Event != nil && !o.FullReport && WakeCapable(kind) {
		return o.Event.Text(), DeliveryEvent
	}
	if o.Compose != nil {
		text = o.Compose(text)
	}
	return text, DeliveryFull
}
