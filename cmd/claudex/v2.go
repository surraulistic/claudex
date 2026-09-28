package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/exitcode"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/supervise"
	"github.com/surraulistic/claudex/internal/task"
)

// Публичная поверхность ClauDex.
//
// Интерфейс рос кусками: delegate, tell, tasks, digest, flush, reconcile — и
// по именам уже не читается, что из этого рабочий обиход, а что починка.
// Здесь он сводится к одной модели: `send` отправляет работу, `task` отвечает
// на «ну что там», `doctor` проверяет, цела ли обвязка.
//
// Старые имена остаются и работают: у них есть вызывающие, которых мы не
// видим, и ломать их ради стройности нечестно.
//
// Кто затеял поручение, `send` не спрашивает. Возврат результата тому, кто
// затеял, — свойство самого поручения, а не особенность Codex: адрес
// выбирается из окружения по убыванию точности (тред Codex, разговор Claude
// Code, панель herdr), и отдельного флага под каждого вызывающего нет.

// cmdSend — отправить работу.
//
// От delegate отличается умолчанием: send не занимает ход ожиданием. Ждать
// приходится редко, а занятый ход стоит дорого — поэтому ожидание стало
// явным (--wait), а не поведением по умолчанию.
func cmdSend(o opts, tgt, prompt string) error {
	if o.headless {
		return headlessNotReady(tgt, prompt)
	}
	if o.newSession {
		return exitcode.Wrap(exitcode.BadCall, errors.New(
			"--new пока не реализован: он заведёт нового работника вместо отправки существующему; "+
				"сейчас назовите цель явно или воспользуйтесь --headless, когда он появится"))
	}
	// Ожидание — явное. Всё остальное send берёт у delegate как есть: журнал,
	// адрес возврата, дедупликация и дожим у них общие.
	o.noWait = !o.wait
	if o.session {
		// Явный флаг остаётся отдушиной для скриптов: он снимает всякую
		// двусмысленность ценой многословности.
		return cmdDelegateSession(o, tgt, prompt)
	}

	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return err
	}
	a, err := task.Classify(tgt, claudesess.Home(), recs)
	if err != nil {
		return exitcode.Wrap(exitcode.BadCall, err)
	}
	switch a.Kind {
	case task.AddrSession:
		return delegateToConversation(o, a.Session, "", prompt)
	case task.AddrTask:
		return sendContinuation(o, a.Task, recs, prompt)
	default:
		return cmdDelegate(o, tgt, prompt)
	}
}

// sendContinuation дописывает работу тому же исполнителю.
//
// Отдельного поручения не заводит: это продолжение уже начатого, и его
// жизненный цикл остаётся за исходным поручением. Завести второе значило бы
// разорвать отчётность надвое.
func sendContinuation(o opts, id string, recs []journal.Record, prompt string) error {
	who, ok := task.ExecutorOfTask(recs, id)
	if !ok {
		return exitcode.Errorf(exitcode.NotFound,
			"у поручения %s не записан разговор-исполнитель: продолжать некому", id)
	}
	s, err := claudesess.Lookup(claudesess.Home(), who)
	if err != nil {
		return exitcode.Errorf(exitcode.NotFound,
			"исполнитель поручения %s больше не жив: %v", id, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudesess.SendTimeout)
	defer cancel()
	if err := claudesess.Send(ctx, s, prompt); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	fmt.Printf("продолжение поручения %s ушло исполнителю %s (%s)\n",
		id, s.Short(), nameOr(s))
	fmt.Printf("  отчётность остаётся за %s: claudex task %s\n", id, id)
	return nil
}

// headlessNotReady — почему --headless пока отказывает.
//
// Заглушка намеренно многословна: рабочие headless-сессии уже запускают мимо
// ClauDex напрямую через `claude --bg`, и такая работа не попадает ни в
// журнал, ни в сборку молчаливых отчётов — она просто теряется. Отказ должен
// называть это, а не молчать.
func headlessNotReady(tgt, prompt string) error {
	_ = tgt
	_ = prompt
	return exitcode.Wrap(exitcode.BadCall, errors.New(
		"--headless пока не реализован.\n"+
			"Замысел: claudex заводит фоновую сессию Claude Code (claude --bg), "+
			"записывает её разговор в журнал как исполнителя и дальше ведёт поручение "+
			"обычным путём — отчёт, сборка молчаливых, дожим.\n"+
			"Почему это нужно: `claude --bg` в обход claudex не оставляет следа — "+
			"ни состояния, ни отчёта, ни возврата затеявшему.\n"+
			"Пока пользуйтесь `claudex send <цель>` к живой сессии."))
}

// cmdTask — «ну что там».
//
//	claudex task list          какие поручения ещё ждут
//	claudex task <id>          состояние одного
//	claudex task log <id>      подробный ход работы (digest — прежнее имя)
func cmdTask(o opts, args []string) error {
	if len(args) == 0 {
		return cmdTaskList(o)
	}
	switch args[0] {
	case "list", "ls":
		return cmdTaskList(o)
	case "log", "digest":
		if len(args) < 2 {
			return exitcode.Wrap(exitcode.BadCall,
				errors.New("нужен идентификатор: claudex task log <id>"))
		}
		return cmdTaskDigest(o, args[1])
	}
	if !task.IsTaskID(args[0]) {
		return exitcode.Errorf(exitcode.BadCall,
			"%q не похоже на идентификатор поручения и не является подкомандой (list, log)", args[0])
	}
	return cmdTaskOne(o, args[0])
}

// cmdTaskList показывает только то, чего ещё ждут. Весь журнал целиком
// остаётся за `claudex tasks --all`: на вопрос «что происходит сейчас» он не
// отвечает, а контекст занимает.
func cmdTaskList(o opts) error {
	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return err
	}
	open := task.Open(recs)
	if o.all {
		open = task.OpenAll(recs)
	}
	if o.pretty {
		return emit(o, map[string]any{"open": open, "count": len(open)})
	}
	if len(open) == 0 {
		fmt.Println("ожидающих поручений нет")
		return nil
	}
	for _, l := range open {
		fmt.Printf("  %s  %-12s %-26s %s\n", l.Task, l.State,
			cutTo(l.Executor, 26), cutTo(oneLine(l.Reason), 44))
	}
	if !o.all {
		if n := len(task.OpenAll(recs)) - len(open); n > 0 {
			fmt.Printf("\nещё %d брошенных (без движения дольше трёх суток): claudex task list --all\n", n)
		}
	}
	fmt.Printf("подробнее: claudex task <id> · claudex task log <id>\n")
	return nil
}

func cmdTaskOne(o opts, id string) error {
	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return err
	}
	l := task.StateOf(recs, id)
	if o.pretty {
		return emit(o, l)
	}
	fmt.Printf("%s  %s\n", l.Task, l.State)
	if !l.Since.IsZero() {
		fmt.Printf("  с %s\n", l.Since.Format("02.01 15:04:05"))
	}
	if l.Executor != "" {
		fmt.Printf("  исполнитель: %s\n", l.Executor)
	}
	if l.Caller != "" {
		how := "событием, читает сам"
		if l.Delivery == task.DeliveryFull {
			how = "отчётом целиком"
		}
		fmt.Printf("  вернуть: %s (%s, %s)\n", l.Caller, l.CallerKind, how)
	}
	if l.Reason != "" {
		fmt.Printf("  %s\n", l.Reason)
	}
	switch l.State {
	case task.StateUndelivered:
		fmt.Println("  отчёт цел, но затеявшего не достиг: claudex flush")
	case task.StateSent:
		fmt.Println("  отчёта ещё не было: claudex task log " + id)
	}
	return nil
}

// cmdDoctor — цела ли обвязка.
//
// Нужен потому, что отказы ClauDex почти всегда упираются во внешнее: herdr не
// отвечает, реестр разговоров пуст, индекс отстал. Разбираться в этом по
// одному отказу за раз дорого.
func cmdDoctor(o opts) error {
	type check struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Note string `json:"note"`
	}
	var out []check
	add := func(n string, ok bool, f string, a ...any) {
		out = append(out, check{Name: n, OK: ok, Note: fmt.Sprintf(f, a...)})
	}

	if agents, err := client().Agents(); err != nil {
		add("herdr", false, "не отвечает: %v — панельная адресация недоступна", err)
	} else {
		add("herdr", true, "%d панелей", len(agents))
	}

	live := claudesess.List(claudesess.Home())
	add("разговоры Claude Code", len(live) > 0, "%d живых в %s", len(live), claudesess.Home())

	if t := codex.ThreadID(); t != "" {
		add("тред Codex", true, "%s — отчёт вернётся сюда", t[:8])
	} else if s := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")); s != "" {
		add("разговор-затейник", true, "%s — отчёт вернётся сюда", s[:8])
	} else if p := os.Getenv("HERDR_PANE_ID"); p != "" {
		add("панель-затейник", true, "%s — отчёт вернётся сюда", p)
	} else {
		add("кому возвращать", false, "ни треда, ни разговора, ни панели: отчёт будет некому отдать")
	}

	if task.WakeCapable(callerKind()) {
		add("способ пробуждения", true,
			"событие: затеявший читает поручение сам (claudex task / task log)")
	} else {
		add("способ пробуждения", true,
			"отчёт целиком: этот адресат читать сам не умеет — так и задумано")
	}

	if st, alive := supervise.Read(supervise.StatusPath()); alive {
		add("наблюдатель", true,
			"работает (pid %d, тиков %d, последний %s назад): недоставленное досылается само",
			st.PID, st.Ticks, time.Since(st.LastTick).Round(time.Second))
	} else {
		add("наблюдатель", false,
			"не работает: недоставленное уйдёт только при следующем вызове claudex — "+
				"запустить фоном: nohup claudex supervisor >>~/.claudex/supervisor.log 2>&1 &")
	}

	jp := defaultJournal()
	if fi, err := os.Stat(jp); err == nil && fi.Size() > 4<<20 {
		add("размер журнала", false,
			"%.1f МБ и растёт: архивация включается флагом наблюдателя --archive-after 30",
			float64(fi.Size())/(1<<20))
	}
	recs, err := journal.Open(jp).Read()
	if err != nil {
		add("журнал", false, "%s: %v", jp, err)
	} else {
		add("журнал", true, "%d записей, ожидающих поручений %d", len(recs), len(task.Open(recs)))
		if lost := task.LostReports(recs); len(lost) > 0 {
			add("недоставленные отчёты", false, "%d — досылает наблюдатель", len(lost))
		} else {
			add("недоставленные отчёты", true, "нет")
		}
		// Отказы в отправке видно только здесь: поручения они не заводят, и
		// task list про них не знает — он идёт по заведённым.
		if ref := task.RefusedTasks(recs); len(ref) > 0 {
			add("не отправлено вовсе", false,
				"%d текстов сохранено, но никуда не ушло: claudex undelivered", len(ref))
		} else {
			add("не отправлено вовсе", true, "нет")
		}
	}

	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".claudex")); err != nil {
		add("каталог состояния", false, "~/.claudex: %v", err)
	}

	if o.pretty {
		return emit(o, map[string]any{"checks": out})
	}
	bad := 0
	for _, c := range out {
		mark := "  ок  "
		if !c.OK {
			mark, bad = "  ⚠   ", bad+1
		}
		fmt.Printf("%s%-24s %s\n", mark, c.Name, c.Note)
	}
	if bad > 0 {
		return exitcode.Errorf(exitcode.Fail, "проверок с замечаниями: %d", bad)
	}
	return nil
}

// cmdSession — кому можно отправлять.
//
//	claudex session list   живые разговоры и их идентификаторы
//
// Отдельно от `task list` намеренно: тот показывает уже заведённые работы, а
// этот — адресатов, которым работу можно дать. Путать их дорого: «какие есть
// работники» и «что я уже поручил» — разные вопросы.
//
// Имя панели herdr и её метка — удобство для человека и годятся, чтобы найти
// адресата глазами. Рабочий путь — идентификатор разговора: имя панели
// переживает смену агента, а идентификатор всегда указывает на один и тот же
// разговор.
func cmdSession(o opts, args []string) error {
	if len(args) > 0 && args[0] != "list" && args[0] != "ls" {
		return exitcode.Errorf(exitcode.BadCall,
			"%q — не подкоманда (list)", args[0])
	}
	return cmdPeers(o)
}

// callerKind — чем является тот, кто нас позвал. Для doctor: способ
// пробуждения зависит именно от этого.
func callerKind() string {
	if codex.ThreadID() != "" {
		return task.KindThread
	}
	if strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID")) != "" {
		return task.KindSession
	}
	return task.KindPane
}

// cmdArchive уносит из журнала поручения, по которым давно ничего не
// происходит. Старое не удаляется, а переезжает: разбор инцидента
// полугодовой давности — обычное дело.
func cmdArchive(o opts, args []string) error {
	days := 30
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 {
			return exitcode.Errorf(exitcode.BadCall,
				"нужно число дней: claudex archive [дней] [--dry-run]")
		}
		days = n
	}
	res, err := journal.Archive(defaultJournal(), time.Duration(days)*24*time.Hour, o.dryRun)
	if err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	if o.pretty {
		return emit(o, res)
	}
	if res.Moved == 0 {
		fmt.Printf("переносить нечего: всё, что в журнале, свежее %s\n", res.SinceDay)
		return nil
	}
	what := "перенесено"
	if res.DryRun {
		what = "перенеслось бы"
	}
	fmt.Printf("%s %d записей по %d поручениям (старше %s), останется %d\n",
		what, res.Moved, res.Tasks, res.SinceDay, res.Kept)
	if res.Path != "" {
		fmt.Printf("архив: %s\n", res.Path)
	}
	return nil
}
