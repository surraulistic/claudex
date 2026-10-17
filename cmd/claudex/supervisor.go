package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/surraulistic/claudex/internal/exitcode"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/supervise"
)

// `supervisor` доводит отчёты до затеявшего сам.
//
// Имя выбрано против «monitor» намеренно: наблюдатель не только смотрит, он
// действует — досылает то, что не дошло. «monitor» обещал бы наблюдение без
// вмешательства, и первая же доставка это обещание нарушила бы.
//
// Присматривает он за поручениями, а не за работниками: запускать и
// перезапускать исполнителей он не умеет и не должен.

func cmdSupervisor(o opts, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return supervisorStatus(o)
		case "stop":
			return supervisorStop()
		default:
			return exitcode.Errorf(exitcode.BadCall,
				"%q — не подкоманда наблюдателя (status, stop)", args[0])
		}
	}

	if st, alive := supervise.Read(supervise.StatusPath()); alive {
		return exitcode.Errorf(exitcode.Busy,
			"наблюдатель уже работает (pid %d, тиков %d): второй будет дублировать доставку",
			st.PID, st.Ticks)
	}
	// Несколько попыток: ensureSupervisor проверяет замок, на миг беря его
	// сам, и стартующий наблюдатель может попасть ровно в этот миг.
	release, err := supervise.Acquire(supervise.LockPath())
	for i := 0; errors.Is(err, supervise.ErrRunning) && i < 10; i++ {
		time.Sleep(30 * time.Millisecond)
		release, err = supervise.Acquire(supervise.LockPath())
	}
	if err != nil {
		return exitcode.Errorf(exitcode.Busy,
			"%v: второй будет дублировать доставку", err)
	}
	defer release()

	s := supervise.New(supervise.Options{
		Journal: journal.Open(defaultJournal()), Client: client(),
		StatusPath: supervise.StatusPath(),
		Interval:   o.interval, MaxPerTick: o.maxPerTick, Reconcile: o.reconcile,
		SilenceGrace: o.silence,
		ArchiveAfter: o.archiveAfter, JournalPath: defaultJournal(),
	})
	if o.once {
		t := s.Once(context.Background())
		if o.pretty {
			return emit(o, t)
		}
		fmt.Printf("доставлено %d, отложено %d, ждут %d\n",
			len(t.Delivered), t.Deferred, t.Pending)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr,
		"наблюдатель запущен: pid %d, интервал %s, за тик не больше %d\n"+
			"состояние: %s · остановить: claudex supervisor stop\n",
		os.Getpid(), s.Status().Interval, supervise.DefaultMaxPerTick, supervise.StatusPath())
	if err := s.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func supervisorStatus(o opts) error {
	st, alive := supervise.Read(supervise.StatusPath())
	if o.pretty {
		return emit(o, map[string]any{"running": alive, "status": st})
	}
	if !alive {
		fmt.Println("наблюдатель не работает — отчёты досылаются только при вызовах claudex")
		fmt.Println("запустить фоном: nohup claudex supervisor >>~/.claudex/supervisor.log 2>&1 &")
		return nil
	}
	fmt.Printf("работает: pid %d, тиков %d, интервал %s\n", st.PID, st.Ticks, st.Interval)
	fmt.Printf("  доставлено %d, отказов %d, ждут %d, отложено %d\n",
		st.Delivered, st.Failed, st.Pending, st.Deferred)
	fmt.Printf("  последний тик: %s назад\n", time.Since(st.LastTick).Round(time.Second))
	if st.ArchiveNote != "" {
		fmt.Printf("  архив: %s\n", st.ArchiveNote)
	}
	return nil
}

// ensureSupervisor starts the ordinary non-LLM observer when a headless task
// needs its Stop-hook fallback. It is intentionally idempotent.
func ensureSupervisor(_ opts) error {
	if supervisorRunning() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	statusPath := supervise.StatusPath()
	if statusPath == "" {
		return errors.New("не удалось определить путь состояния наблюдателя")
	}
	logPath := filepath.Join(filepath.Dir(statusPath), "supervisor.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(self, "supervisor")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("запустить наблюдатель: %w", err)
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if supervisorRunning() {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("наблюдатель не подтвердил запуск; подробности в %s", logPath)
}

// supervisorRunning — замок или, для наблюдателя прежней сборки без замка,
// живой процесс из файла состояния.
func supervisorRunning() bool {
	if supervise.Held(supervise.LockPath()) {
		return true
	}
	_, alive := supervise.Read(supervise.StatusPath())
	return alive
}

// autoSupervise поднимает наблюдателя для любого отслеживаемого send и done:
// без него отчёт, принятый в очередь треда, так и не превратится в ход.
// Отказ не валит команду — она своё уже сделала.
func autoSupervise() {
	if os.Getenv("CLAUDEX_NO_AUTOSUPERVISOR") != "" {
		return
	}
	if err := ensureSupervisor(opts{}); err != nil {
		fmt.Fprintf(os.Stderr, "claudex: наблюдатель не запущен: %v\n", err)
	}
}

// supervisorStop шлёт сигнал только держателю замка: PID из файла состояния
// мог уже достаться другому процессу.
func supervisorStop() error {
	pid, held := supervise.Holder(supervise.LockPath())
	if !held {
		if st, alive := supervise.Read(supervise.StatusPath()); alive {
			return exitcode.Errorf(exitcode.Fail,
				"в %s записан pid %d, но замок наблюдателя никто не держит: это может быть уже "+
					"другой процесс — не останавливаю; проверьте его сами (ps -p %d)",
				supervise.StatusPath(), st.PID, st.PID)
		}
		fmt.Println("наблюдатель не работает")
		return nil
	}
	if pid <= 0 {
		return exitcode.Errorf(exitcode.Fail,
			"замок наблюдателя занят, но pid в нём не записан (наблюдатель прежней сборки): "+
				"остановите его вручную")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	fmt.Printf("остановлен (pid %d)\n", pid)
	return nil
}
