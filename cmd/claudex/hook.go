package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/surraulistic/claudex/internal/exitcode"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// Завершение хода как событие платформы.
//
// Прежде единственным признаком «задача доделала» был вызов `claudex done`,
// который задача обязана помнить. Замерено на журнале: помнит в шести случаях
// из десяти — 168 поручений из 424 пропали молча, а сборка с экрана не собрала
// ни одного.
//
// Claude Code сообщает об окончании хода сам, хуком Stop, и отдаёт вместе с
// ним текст последней реплики — в его схеме так и написано: избавляет от
// разбора стенограммы. Уговаривать модель помнить команду ради того, что
// платформа и так шлёт, — и есть костыль.
//
// Окончание хода завершением поручения не является: ход кончается и посреди
// работы, и когда задача задаёт вопрос. Поэтому хук ничего не доставляет и
// никого не будит — он только записывает наблюдение. Решает наблюдатель, по
// тому, сколько исполнитель молчит после этого.

// stopPayload — то, что Claude Code кладёт хуку на stdin.
type stopPayload struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Last      string `json:"last_assistant_message"`
	Active    bool   `json:"stop_hook_active"`
	// Background — незавершённая фоновая работа. Ради неё поле и читается:
	// оно отличает «сессия закончила» от «сессия ждёт фоновую задачу», и без
	// него ожидание принималось бы за конец работы.
	Background []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"background_tasks"`
}

func cmdHook(o opts, args []string) error {
	if len(args) == 0 {
		return exitcode.Errorf(exitcode.BadCall,
			"нужно: claudex hook stop  (вызывается хуком Claude Code, не руками)")
	}
	switch args[0] {
	case "stop":
		return hookStop(o)
	case "install":
		return hookInstallHint()
	default:
		return exitcode.Errorf(exitcode.BadCall, "%q — не событие (stop, install)", args[0])
	}
}

// hookStop молчалив по устройству: он выполняется внутри чужого хода, и любая
// его строка попадёт человеку на экран.
func hookStop(o opts) error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var p stopPayload
	if json.Unmarshal(raw, &p) != nil || p.SessionID == "" {
		return nil
	}
	for _, b := range p.Background {
		if b.Status == "running" || b.Status == "pending" {
			// Сессия не закончила, а ждёт фоновую работу.
			return nil
		}
	}

	j := journal.Open(defaultJournal())
	recs, err := j.Read()
	if err != nil {
		return nil
	}
	// Отметка нужна только там, где её кто-то ждёт: без открытого поручения
	// запись про каждый ход превратила бы журнал в лог ходов.
	open := task.ActiveFor(recs, p.SessionID)
	if len(open) == 0 {
		return nil
	}
	for _, a := range open {
		j.Append(journal.Record{
			Task: a.Task, Event: journal.Idle, PaneSession: p.SessionID,
			Reason: strings.TrimSpace(p.Last),
		})
	}
	return nil
}

func hookInstallHint() error {
	fmt.Print(`Добавьте в ~/.claude/settings.json, в раздел hooks:

  "Stop": [
    { "hooks": [ { "type": "command",
                   "command": "` + selfBinaryPath() + ` hook stop",
                   "async": true } ] }
  ]

Хук выполняется в сессии-исполнителе, ничего не печатает и никого не будит:
он записывает, что ход кончился и что задача сказала последней репликой.
Разбудит наблюдатель, когда молчание станет окончательным.
`)
	return nil
}

func selfBinaryPath() string {
	p, err := os.Executable()
	if err != nil {
		return "claudex"
	}
	return p
}
