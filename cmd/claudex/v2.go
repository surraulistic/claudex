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
	"github.com/surraulistic/claudex/internal/headless"
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
func cmdSend(o opts, tgt, prompt string) (err error) {
	defer func() {
		if err == nil {
			autoSupervise()
		}
	}()
	if o.headless {
		if o.wait {
			return exitcode.Errorf(exitcode.BadCall,
				"--headless всегда возвращает управление сразу; результат придёт отдельным событием")
		}
		return cmdHeadless(o, tgt, prompt)
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
		// Отправить работнику, у которого работа уже идёт, — это чаще всего
		// продолжение, а не второе поручение. Прежде каждая отправка заводила
		// отдельное, и у одного исполнителя их накопилось 62.
		id, err := continueOrStart(recs, a.Session.ID, o.newSession)
		if err != nil {
			return exitcode.Wrap(exitcode.BadCall, err)
		}
		if id != "" {
			return sendContinuation(o, id, recs, prompt)
		}
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
func sendContinuation(o opts, id string, recs []journal.Record, prompt string) (err error) {
	var sendErr error
	who, ok := task.ExecutorOfTask(recs, id)
	if !ok {
		return exitcode.Errorf(exitcode.NotFound,
			"у поручения %s не записан разговор-исполнитель: продолжать некому", id)
	}
	j := journal.Open(defaultJournal())
	attempt, err := task.BeginContinuation(j, id, who, prompt)
	if err != nil {
		return exitcode.Errorf(exitcode.Fail, "продолжение не записано в журнал, не отправляю: %v", err)
	}
	defer func() { task.EndContinuation(j, id, attempt, sendErr) }()
	if sessionID, cwd, headlessWorker := task.HeadlessExecutor(recs, id); headlessWorker {
		session, err := headless.Resume(context.Background(), cwd, id, sessionID,
			task.WithReport(prompt, "", id))
		if err != nil {
			sendErr = err
			return exitcode.Wrap(exitcode.Fail, err)
		}
		if session.SessionID != sessionID {
			var binding journal.Record
			for _, r := range recs {
				if r.Task == id && r.Event == journal.Started && r.Headless {
					binding = r
				}
			}
			binding.Time = time.Time{}
			binding.PaneSession = session.SessionID
			if err := j.Append(binding); err != nil {
				return exitcode.Wrap(exitcode.Fail, err)
			}
		}
		fmt.Printf("продолжение поручения %s ушло в headless Claude %s; отчётность остаётся за исходным поручением\n",
			id, session.ID)
		return nil
	}
	s, err := claudesess.Lookup(claudesess.Home(), who)
	if err != nil {
		sendErr = fmt.Errorf("исполнитель больше не жив: %w", err)
		return exitcode.Errorf(exitcode.NotFound,
			"исполнитель поручения %s больше не жив: %v", id, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudesess.SendTimeout)
	defer cancel()
	if err := claudesess.Send(ctx, s, prompt); err != nil {
		sendErr = err
		return exitcode.Wrap(exitcode.Fail, err)
	}
	fmt.Printf("продолжение поручения %s ушло исполнителю %s (%s)\n",
		id, s.Short(), nameOr(s))
	fmt.Printf("  отчётность остаётся за %s: claudex task %s\n", id, id)
	return nil
}

func cmdHeadless(o opts, targetName, prompt string) error {
	j := journal.Open(defaultJournal())
	if err := wakeIsPossible(j, o); err != nil {
		return err
	}
	wake := task.ResolveWake(client(), o.notify, o.notifyThread, o.notifySession)
	if err := validateHeadlessWake(j, wake); err != nil {
		return err
	}
	inlineUpkeep(o)

	recs, err := j.Read()
	if err != nil {
		return err
	}
	cwd, err := headlessCWD(targetName, recs)
	if err != nil {
		return exitcode.Wrap(exitcode.BadCall, err)
	}
	id := task.NewID()
	started := journal.Record{
		Task: id, Event: journal.Started,
		Headless: true, CWD: cwd, Target: wake.Target,
		TargetSession: wake.Session, TargetKind: wake.Kind,
		Prompt: prompt,
	}
	// Record task intent before launch. Claude assigns its own session UUID;
	// after --bg returns, bind the exact ID reported by `claude agents`.
	if err := j.Append(started); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}

	workerPrompt := task.WithReport(prompt, "", id)
	session, err := headless.Start(context.Background(), cwd, id, "", workerPrompt)
	if err != nil {
		j.Append(journal.Record{Task: id, Event: journal.LaunchFailed,
			Outcome: "headless запуск не удался", Reason: err.Error(), Prompt: prompt})
		return exitcode.Wrap(exitcode.Fail, err)
	}
	started.Time = time.Time{}
	started.PaneSession = session.SessionID
	if err := j.Append(started); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	if err := j.Append(journal.Record{Task: id, Event: journal.Finished,
		Outcome: task.SentNoWait}); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}

	if o.pretty {
		return emit(o, map[string]any{
			"task": id, "executor": map[string]any{"kind": "claude-background",
				"id": session.ID, "session_id": session.SessionID, "name": session.Name,
				"cwd": session.CWD},
			"state": task.StateSent, "return_to": wake,
		})
	}
	fmt.Printf("поручение %s запущено в headless Claude %s (%s)\n",
		id, session.ID, cwd)
	fmt.Printf("результат вернётся сюда; полный ход: claudex task log %s\n", id)
	fmt.Printf("наблюдать: claude attach %s · последние строки: claude logs %s\n",
		session.ID, session.ID)
	return nil
}

func validateHeadlessWake(j *journal.Journal, wake task.Wake) error {
	if wake.Empty() {
		return exitcode.Errorf(exitcode.BadCall,
			"некуда вернуть результат: запустите claudex из Codex или Claude Code")
	}
	switch wake.Kind {
	case task.KindThread:
		if v := task.MayQueue(wake.Target, codex.State(codex.Home(), wake.Target)); !v.OK() {
			return exitcode.Errorf(exitcode.BadCall, "результат будет некуда доставить: %s", v.Reason())
		}
	case task.KindSession:
		if _, err := claudesess.Lookup(claudesess.Home(), wake.Target); err != nil {
			return exitcode.Errorf(exitcode.BadCall,
				"разговор-отправитель для результата недоступен: %v", err)
		}
	case task.KindPane:
		if v := task.MayWrite(wake.Target, wake.Session, wake.Session,
			task.SharedPane(j, wake.Target)); !v.OK() {
			return exitcode.Errorf(exitcode.BadCall,
				"некуда безопасно вернуть результат: %s", v.Reason())
		}
	default:
		return exitcode.Errorf(exitcode.BadCall,
			"неизвестный адрес возврата результата: %q", wake.Kind)
	}
	return nil
}

func headlessCWD(targetName string, recs []journal.Record) (string, error) {
	if strings.TrimSpace(targetName) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("не удалось определить текущую папку: %w", err)
		}
		return cwd, nil
	}
	if info, err := os.Stat(targetName); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("%q — не папка; для headless укажите проект, разговор или панель", targetName)
		}
		return filepath.Abs(targetName)
	}
	a, err := task.Classify(targetName, claudesess.Home(), recs)
	if err != nil {
		return "", err
	}
	switch a.Kind {
	case task.AddrSession:
		if a.Session.CWD == "" {
			return "", fmt.Errorf("у разговора %s нет известной рабочей папки", a.Session.Short())
		}
		return a.Session.CWD, nil
	case task.AddrTask:
		if _, cwd, ok := task.HeadlessExecutor(recs, a.Task); ok && cwd != "" {
			return cwd, nil
		}
		return "", fmt.Errorf("поручение %s не содержит рабочего каталога для нового headless-сеанса", a.Task)
	default:
		p, err := resolve(targetName)
		if err != nil {
			return "", err
		}
		if p.CWD == "" {
			return "", fmt.Errorf("у панели %s нет известной рабочей папки", p.ID)
		}
		return p.CWD, nil
	}
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
	printThreadTimeline(recs, id)
	printContinuations(recs, id)
	switch l.State {
	case task.StateQueued:
		fmt.Println("  событие в очереди треда Codex, ход с ним не начат — это ещё не обработка")
	case task.StateUndelivered:
		if turnFailed(recs, id) {
			fmt.Println("  ход треда с событием оборвался: повторно не кладём, чтобы не повторить сделанное —" +
				" напишите в тред сами")
			break
		}
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

	if s, err := dialAppServer(); err != nil {
		add("app-server Codex", false,
			"недоступен (%v): отчёт в тред ляжет в очередь, но ход сам не начнётся", err)
	} else {
		s.Close()
		add("app-server Codex", true, "%s — выгруженный тред будится ходом", codex.Socket())
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
		lost := task.LostReports(recs)
		live := task.LivePending(recs, time.Now())
		var failedTurns int
		for _, l := range lost {
			if l.Cause == task.CauseTurnFailed {
				failedTurns++
			}
		}
		if failedTurns > 0 {
			add("ход треда оборвался", false,
				"%d — событие могло остаться необработанным, повторно не кладём: claudex undelivered", failedTurns)
		}
		lost = lost[:0:0]
		for _, l := range task.LostReports(recs) {
			if l.Cause != task.CauseTurnFailed {
				lost = append(lost, l)
			}
		}
		switch {
		case len(live) > 0:
			add("недоставленные отчёты", false, "%d — досылает наблюдатель", len(live))
		case len(lost) > 0:
			// Попытки прекращены, но отчёты целы: это не «всё хорошо», а
			// «дальше бесполезно».
			add("недоставленные отчёты", true,
				"свежих нет; %d брошено — адреса молчат дольше трёх суток, текст цел: claudex undelivered",
				len(lost))
		default:
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

	b := buildInfo()
	drift := 0
	for _, c := range skillCopies() {
		if !c.Matches {
			drift++
		}
	}
	add("сборка", b.Source == "build" && drift == 0,
		"ревизия %s (%s), skill расходится с бинарём: %d — подробнее: claudex version", short12(b.Revision), b.Source, drift)

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
func dialAppServer() (codex.AppServer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return codex.Dial(ctx)
}

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

// continueOrStart решает, продолжать ли уже идущую работу.
//
// Пустой идентификатор значит «завести новое». Отправить работнику, у которого
// работа идёт, — это чаще всего продолжение: прежде каждая отправка заводила
// отдельное поручение, и у одного исполнителя их накопилось 62, ни одно не
// закрыто. Несколько незакрытых — отказ: какое продолжают, знает только человек.
func continueOrStart(recs []journal.Record, session string, forceNew bool) (string, error) {
	if forceNew {
		return "", nil
	}
	switch open := task.OpenFor(recs, session); len(open) {
	case 0:
		return "", nil
	case 1:
		return open[0].Task, nil
	default:
		return "", &task.TooManyOpen{Who: session, Open: open}
	}
}

func turnFailed(recs []journal.Record, id string) bool {
	for _, l := range task.LostReports(recs) {
		if l.Task == id && l.Cause == task.CauseTurnFailed {
			return true
		}
	}
	return false
}
