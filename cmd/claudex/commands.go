package main

import (
	"os"
	"strings"

	"github.com/surraulistic/claudex/internal/exitcode"
)

// Перечень команд таблицей, а не switch-ем.
//
// Затем, что списков было два — разбор и справка, — и они расходились. Уже
// дважды: `--notify-session` не попал в справку вовсе, а `session list` и
// `--panel` оказались только в примерах. Пока согласие двух списков держится на
// внимательности, оно теряется; таблица делает его проверяемым, и проверка
// стоит в тестах.
//
// Здесь же видно устройство поверхности: что обиход, что прежнее имя, что
// служебное. Раньше это знание жило только в голове.

type command struct {
	Name    string
	Aliases []string
	// Legacy — прежнее имя. Работает и останется: у него есть вызывающие,
	// которых мы не видим. В справке стоит отдельно, чтобы новые вызовы на
	// него не заводили.
	Legacy bool
	// Internal — служебное или починка: в обиходе не нужно.
	Internal bool
	// MinArgs — сколько доводов обязательно после имени.
	MinArgs int
	// Need — что сказать, когда доводов мало. Пусто значит «их и не нужно».
	Need string
	Run  func(o opts, args []string) error
}

func commands() []command {
	return []command{
		{Name: "send", MinArgs: 1, Need: `нужно: claudex send [проект] "<задача>"`,
			Run: cmdSendArgs},
		{Name: "task", Run: cmdTask},
		{Name: "session", Aliases: []string{"sessions-live"}, Run: cmdSession},
		{Name: "doctor", Run: func(o opts, _ []string) error { return cmdDoctor(o) }},
		{Name: "supervisor", Run: cmdSupervisor},
		{Name: "done", Run: cmdDone},

		{Name: "sessions", Run: func(o opts, _ []string) error { return cmdSessions(o, false) }},
		{Name: "brief", Run: func(o opts, _ []string) error { return cmdSessions(o, true) }},
		{Name: "find", Run: func(o opts, a []string) error { return cmdFind(o, strings.Join(a, " ")) }},
		{Name: "search", MinArgs: 2, Need: "нужны цель и запрос",
			Run: func(o opts, a []string) error { return cmdSearch(o, a[0], strings.Join(a[1:], " ")) }},
		{Name: "entry", Run: cmdEntry},
		{Name: "context", Run: cmdContext},
		{Name: "watch", Run: cmdWatch},
		{Name: "index", Run: func(o opts, _ []string) error { return cmdIndex(o) }},
		{Name: "schema", Run: func(opts, []string) error { return cmdSchema() }},

		{Name: "hook", Internal: true, Run: cmdHook},
		{Name: "archive", Internal: true, Run: cmdArchive},
		{Name: "tell", Internal: true, Run: cmdTell},
		{Name: "flush", Internal: true, Run: func(o opts, _ []string) error { return cmdFlush(o) }},
		{Name: "reconcile", Internal: true, Run: func(o opts, _ []string) error { return cmdReconcile(o) }},
		{Name: "undelivered", Internal: true, Run: func(o opts, _ []string) error { return cmdUndelivered(o) }},

		{Name: "delegate", Legacy: true, MinArgs: 2, Need: "нужны цель и задача",
			Run: func(o opts, a []string) error { return cmdDelegate(o, a[0], strings.Join(a[1:], " ")) }},
		{Name: "tasks", Legacy: true, Run: cmdTasks},
		{Name: "digest", Legacy: true, MinArgs: 1, Need: "нужен идентификатор задачи",
			Run: func(o opts, a []string) error { return cmdTaskDigest(o, a[0]) }},
		{Name: "peers", Legacy: true, Run: func(o opts, _ []string) error { return cmdPeers(o) }},
	}
}

func cmdSendArgs(o opts, args []string) error {
	if o.headless {
		if len(args) == 1 {
			return cmdSend(o, "", args[0])
		}
		return cmdSend(o, args[0], strings.Join(args[1:], " "))
	}
	if len(args) < 2 {
		return exitcode.Errorf(exitcode.BadCall,
			`нужно: claudex send <адресат> "<сообщение>" (для новой фоновой сессии: claudex --headless "<задача>")`)
	}
	return cmdSend(o, args[0], strings.Join(args[1:], " "))
}

func headlessShortcut(o opts, args []string) error {
	target, prompt, err := parseHeadlessArgs(args)
	if err != nil {
		return err
	}
	return cmdSend(o, target, prompt)
}

func parseHeadlessArgs(args []string) (target, prompt string, err error) {
	if len(args) == 0 {
		return "", "", exitcode.Errorf(exitcode.BadCall,
			`нужно: claudex --headless "<задача>"`)
	}
	if len(args) > 1 {
		// Explicit project directory followed by a prompt is useful from a
		// neutral working directory; otherwise every token is prompt text.
		if info, statErr := os.Stat(args[0]); statErr == nil && info.IsDir() {
			return args[0], strings.Join(args[1:], " "), nil
		}
	}
	return "", strings.Join(args, " "), nil
}

// dispatch находит команду по имени. Неизвестное имя — не ошибка: исторически
// `claudex <цель>` показывает дайджест панели, и ломать это нельзя.
func dispatch(o opts, args []string) error {
	for _, c := range commands() {
		if c.Name != args[0] && !has(c.Aliases, args[0]) {
			continue
		}
		if len(args)-1 < c.MinArgs {
			return exitcode.Errorf(exitcode.BadCall, "%s", c.Need)
		}
		return c.Run(o, args[1:])
	}
	if o.headless {
		return headlessShortcut(o, args)
	}
	return cmdDigest(o, args[0])
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
