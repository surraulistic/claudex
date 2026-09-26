package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
	"github.com/surraulistic/claudex/internal/exitcode"
)

// `tell` кладёт текст прямо в разговор Claude Code, минуя панель herdr.
//
// Отличается от `delegate` тем же, чем записка отличается от поручения: в
// журнал не пишется, исполнения не ждёт, отчёта не требует. Нужно ровно для
// того, ради чего адресация по разговору и заводилась — сказать работающей
// сессии то, что ей нужно знать сейчас.

func cmdTell(o opts, args []string) error {
	if len(args) < 2 {
		return exitcode.Wrap(exitcode.BadCall,
			errors.New(`нужно: claudex tell <разговор> "<текст>"`))
	}
	target := args[0]
	text := strings.TrimSpace(strings.Join(args[1:], " "))
	if text == "" {
		return exitcode.Wrap(exitcode.BadCall, errors.New("текст пуст"))
	}

	s, err := claudesess.Resolve(claudesess.Home(), target)
	if err != nil {
		// Разные беды — разные коды: «нет такого» вызывающий переживёт сам,
		// а неоднозначность и пустой адрес чинит только человек.
		if errors.Is(err, claudesess.ErrNotFound) {
			return exitcode.Wrap(exitcode.NotFound, err)
		}
		return exitcode.Wrap(exitcode.BadCall, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), claudesess.SendTimeout)
	defer cancel()
	if err := claudesess.Send(ctx, s, text); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}

	if o.pretty {
		return emit(o, map[string]any{
			"sent": true, "session": s.ID, "name": s.Name, "status": s.Status,
		})
	}
	// Занятость называется прямо: у занятого разговора сообщение прочтётся
	// между вызовами инструментов, а не сейчас, и знать это полезно.
	when := "прочтёт сразу"
	if s.Busy() {
		when = "занят — прочтёт между вызовами инструментов"
	}
	fmt.Printf("отправлено в %s (%s), %s\n", s.Short(), nameOr(s), when)
	return nil
}

// `peers` показывает, кому вообще можно писать. Без него единственный способ
// узнать адрес — заглянуть в чужой каталог, а это уже не интерфейс.
func cmdPeers(o opts) error {
	live := claudesess.List(claudesess.Home())
	if o.pretty {
		return emit(o, map[string]any{"sessions": live, "count": len(live)})
	}
	if len(live) == 0 {
		fmt.Println("живых разговоров нет")
		return nil
	}
	for _, s := range live {
		age := ""
		if !s.StatusAt.IsZero() {
			age = fmt.Sprintf("  %s назад", cutTo(time.Since(s.StatusAt).Round(time.Second).String(), 8))
		}
		fmt.Printf("%s  %-6s %-34s %s%s\n", s.Short(), s.Status, cutTo(nameOr(s), 34), s.CWD, age)
	}
	return nil
}

func nameOr(s claudesess.Session) string {
	if s.Name != "" {
		return s.Name
	}
	return "без имени"
}
