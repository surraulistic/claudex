package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
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

	s := supervise.New(supervise.Options{
		Journal: journal.Open(defaultJournal()), Client: client(),
		StatusPath: supervise.StatusPath(),
		Interval:   o.interval, MaxPerTick: o.maxPerTick, Reconcile: o.reconcile,
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
	return nil
}

func supervisorStop() error {
	st, alive := supervise.Read(supervise.StatusPath())
	if !alive {
		fmt.Println("наблюдатель не работает")
		return nil
	}
	p, err := os.FindProcess(st.PID)
	if err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	fmt.Printf("остановлен (pid %d)\n", st.PID)
	return nil
}
