package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, hookStdinLimit+1))
	if err != nil || len(raw) == 0 {
		return nil
	}
	if len(raw) > hookStdinLimit {
		hookLog("hook stop: stdin больше %d байт, отметка не записана", hookStdinLimit)
		return nil
	}
	var p stopPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		if err != nil {
			hookLog("hook stop: stdin не разобран: %v", err)
		}
		return nil
	}
	for _, b := range p.Background {
		if b.Status == "running" || b.Status == "pending" {
			// Сессия не закончила, а ждёт фоновую работу.
			return nil
		}
	}

	j := journal.Open(defaultJournal())
	// Хук идёт на каждый ход любой сессии, а поручение есть у немногих:
	// сессия, которой в журнале нет вовсе, отметки не получит, и разбирать
	// его целиком незачем.
	if strings.TrimSpace(os.Getenv("CLAUDEX_TASK_ID")) == "" && !journalMentions(j.Path, p.SessionID) {
		return nil
	}
	recs, err := j.Read()
	if err != nil {
		return nil
	}
	if err := bindHeadlessSession(j, recs, p.SessionID); err != nil {
		return nil
	}
	recs, err = j.Read()
	if err != nil {
		return nil
	}
	id, ok := idleMark(recs, p.SessionID, strings.TrimSpace(p.Last))
	if !ok {
		return nil
	}
	if err := j.Append(journal.Record{
		Task: id, Event: journal.Idle, PaneSession: p.SessionID,
		Reason: strings.TrimSpace(p.Last),
	}); err != nil {
		hookLog("hook stop: отметка по %s не записана: %v", id, err)
	}
	return nil
}

// hookStdinLimit — больше Claude Code не присылает; больше — значит, что-то
// не то, и это пишется в hook.log, а не теряется молча.
const hookStdinLimit = 32 << 20

// hookLog пишет в ~/.claudex/hook.log: на экран хуку писать нельзя.
func hookLog(format string, a ...any) {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(defaultJournal()), "hook.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, a...)...)
}

// journalMentions — встречается ли строка в журнале. Ошибка чтения — «да»:
// пусть решает полный разбор.
func journalMentions(path, s string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return bytes.Contains(b, []byte(s))
}

// bindHeadlessSession updates the worker binding when Claude resumes a
// background task under a copied session id. The per-task environment marker
// is injected only into the launched worker's process tree.
func bindHeadlessSession(j *journal.Journal, recs []journal.Record, sessionID string) error {
	taskID := strings.TrimSpace(os.Getenv("CLAUDEX_TASK_ID"))
	if taskID == "" || sessionID == "" {
		return nil
	}
	var started journal.Record
	for _, r := range recs {
		if r.Task == taskID && r.Event == journal.Started {
			started = r
		}
	}
	if started.Event != journal.Started || !started.Headless || started.PaneSession == sessionID {
		return nil
	}
	started.Time = time.Time{}
	started.PaneSession = sessionID
	return j.Append(started)
}

// idleMark — какое поручение пометить по окончании хода и стоит ли вообще.
//
// Отдельной функцией, потому что правило здесь одно, а проверять его надо
// именно то, которым пользуется хук. Повторить его в тесте своими словами —
// значит проверять пересказ: так и вышло с первой версией, и мутация настоящего
// хука тест не роняла.
func idleMark(recs []journal.Record, session, last string) (string, bool) {
	open := task.ActiveFor(recs, session)
	if len(open) != 1 {
		// Ноль — отмечать нечего. Несколько — конец хода не говорит, какое из
		// них закончилось, и пометить все значит записать догадку. На живом
		// журнале у одного исполнителя их было 62: один конец хода разбудил бы
		// затеявшего по каждому.
		return "", false
	}
	id := open[0].Task
	// Старше срока молчание уже не будит, а разговор, который говорит
	// сутками, иначе дописывал бы по реплике на каждый ход.
	if time.Since(open[0].Started) > task.AbandonAfter {
		return "", false
	}
	// Ход кончается много раз подряд, а отметка нужна последняя. Повтор того
	// же текста журналу ничего не добавляет: за час их набралось семь по
	// одному поручению.
	var prev string
	var seen bool
	for _, r := range recs {
		if r.Task == id && r.Event == journal.Idle {
			prev, seen = strings.TrimSpace(r.Reason), true
		}
	}
	if seen && prev == last {
		return "", false
	}
	return id, true
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
