package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/surraulistic/claudex/internal/claudesess"
	"github.com/surraulistic/claudex/internal/exitcode"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

// Поручение разговору, а не панели.
//
// Отличий от панельной ветки два, и оба вытекают из того, что у разговора нет
// экрана. Занятость не мешает отправке: сообщение примут и в середине хода, а
// прочтут между вызовами инструментов — так же, как очередь треда Codex.
// Завершение же доказывается не освобождением панели, а парой переходов в
// реестре: сначала занят, потом свободен. Простой с самого начала завершением
// не считается — разговор попросту ещё не взялся.

// pickupWait — сколько ждать, пока разговор возьмётся за поручение.
const pickupWait = 90 * time.Second

func cmdDelegateSession(o opts, tgt, prompt string) error {
	s, err := claudesess.Resolve(claudesess.Home(), tgt)
	if err != nil {
		if errors.Is(err, claudesess.ErrNotFound) {
			return exitcode.Wrap(exitcode.NotFound, err)
		}
		return exitcode.Wrap(exitcode.BadCall, err)
	}

	j := journal.Open(defaultJournal())
	if err := wakeIsPossible(j, o); err != nil {
		return err
	}
	reconcilePending(o)
	flushPending(o)

	id := task.NewID()
	wake := task.ResolveWake(client(), o.notify, o.notifyThread, o.notifySession)
	// Панели у поручения нет, и подставлять её нечем. Исполнитель записывается
	// там же, где он записывается у панельной ветки, — разговором.
	j.Append(journal.Record{
		Task: id, Event: journal.Started,
		Target: wake.Target, TargetSession: wake.Session, TargetKind: wake.Kind,
		PaneSession: s.ID, Prompt: prompt,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	send, cancel := context.WithTimeout(ctx, claudesess.SendTimeout)
	err = claudesess.Send(send, s, task.WithReport(prompt, "", id))
	cancel()
	if err != nil {
		j.Append(journal.Record{Task: id, Event: journal.Finished,
			Outcome: "не отправлено", Reason: err.Error()})
		return exitcode.Wrap(exitcode.Fail, err)
	}

	if o.noWait {
		fmt.Printf("%s отправлено в %s (%s); отчёт придёт по claudex done\n",
			id, s.Short(), nameOr(s))
		return nil
	}

	outcome, reason := awaitSession(ctx, j, s, id, o.timeout)
	j.Append(journal.Record{Task: id, Event: journal.Finished,
		Outcome: outcome, Reason: reason})
	if o.pretty {
		return emit(o, map[string]any{
			"task": id, "session": s.ID, "outcome": outcome, "reason": reason,
		})
	}
	fmt.Printf("%s %s", id, outcome)
	if reason != "" {
		fmt.Printf(": %s", reason)
	}
	fmt.Println()
	return nil
}

// awaitSession ждёт того, что случится раньше: настоящего отчёта или
// освобождения разговора. Отчёт ценнее — он от самой задачи, а освобождение
// говорит лишь о том, что ход кончился.
func awaitSession(ctx context.Context, j *journal.Journal, s claudesess.Session,
	id string, timeout time.Duration) (string, string) {

	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	reported := make(chan journal.Record, 1)
	go func() {
		if r, err := j.Await(ctx, id, journal.Reported); err == nil {
			reported <- r
		}
	}()
	freed := make(chan error, 1)
	go func() {
		freed <- claudesess.WaitIdle(ctx, claudesess.Home(), s.ID, 2*time.Second, pickupWait)
	}()

	select {
	case r := <-reported:
		return task.Reported, r.Reason
	case err := <-freed:
		if err != nil {
			// И «не взялся», и «пропал» — это «исход неизвестен»: работы мы не
			// видели. Что именно случилось, говорит сам текст ошибки.
			return task.Unknown, err.Error()
		}
		// Разговор отработал и освободился. Отчёт мог не успеть записаться —
		// даём ему короткий срок, иначе быстрая задача выглядит молчаливой.
		grace, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		if r, err := j.Await(grace, id, journal.Reported); err == nil {
			return task.Reported, r.Reason
		}
		return task.Silent, ""
	case <-ctx.Done():
		return task.TimedOut, ctx.Err().Error()
	}
}
