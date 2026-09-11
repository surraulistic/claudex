// Package task — делегирование задачи в чужую панель и ожидание её конца.
//
// Главная трудность не в ожидании, а в сцепке: доказать, что «панель
// освободилась» относится именно к посланной задаче. Экранная метка для этого
// не годится — панель отрисовывает эхо самого задания, и подписка на совпадение
// сработает на нём же. Поэтому делегированный агент отчитывается по боковому
// каналу: дописывает в журнал строку со своим идентификатором задачи и своей
// панелью из HERDR_PANE_ID.
package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
)

const (
	Reported = "отчиталась"
	Silent   = "освободилась без отчёта"
	TimedOut = "не уложилась в срок"
)

var ErrBusy = errors.New("панель занята")

// Состояния, в которых панель считается свободной для нового задания.
var freeStates = map[string]bool{"idle": true, "done": true, "blocked": true, "unknown": true}

type Options struct {
	Client  *herdr.Client
	Journal *journal.Journal
	Pane    string
	Prompt  string
	Timeout time.Duration
	// Grace — сколько ждать отчёт после того, как панель уже освободилась.
	Grace   time.Duration
	Attempt int
}

type Result struct {
	Task     string
	Outcome  string // наша классификация: отчиталась / без отчёта / не уложилась
	Said     string // как сама задача назвала исход
	Reason   string
	Pane     string
	Duration time.Duration
}

func Delegate(ctx context.Context, o Options) (Result, error) {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Minute
	}
	if o.Grace <= 0 {
		o.Grace = 10 * time.Second
	}

	pane, err := o.Client.Get(o.Pane)
	if err != nil {
		return Result{}, err
	}
	if !freeStates[pane.Status] {
		return Result{}, fmt.Errorf("%w: %s в состоянии %q", ErrBusy, o.Pane, pane.Status)
	}

	id := newID()
	started := time.Now()
	res := Result{Task: id, Pane: o.Pane}
	o.Journal.Append(journal.Record{
		Task: id, Event: journal.Started, Pane: o.Pane,
		Prompt: o.Prompt, Attempt: o.Attempt,
	})

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	// Отправка и ожидание освобождения идут одним вызовом herdr, чтобы между
	// ними не было щели, в которую проваливается быстрый ответ.
	freed := make(chan error, 1)
	go func() {
		_, err := o.Client.Prompt(o.Pane, withReportInstruction(o.Prompt, id),
			[]string{"idle", "done", "blocked"}, o.Timeout)
		freed <- err
	}()

	reported := make(chan journal.Record, 1)
	go func() {
		if r, err := o.Journal.Await(ctx, id, journal.Reported); err == nil {
			reported <- r
		}
	}()

	select {
	case r := <-reported:
		res.Outcome, res.Said, res.Reason = Reported, r.Outcome, r.Reason
	case err := <-freed:
		if err != nil {
			res.Outcome, res.Reason = TimedOut, err.Error()
			break
		}
		// Панель освободилась. Отчёт мог не успеть записаться — даём ему срок,
		// иначе быстрая задача выглядит молчаливой.
		grace, stop := context.WithTimeout(ctx, o.Grace)
		if r, err := o.Journal.Await(grace, id, journal.Reported); err == nil {
			res.Outcome, res.Said, res.Reason = Reported, r.Outcome, r.Reason
		} else {
			res.Outcome = Silent
		}
		stop()
	case <-ctx.Done():
		res.Outcome, res.Reason = TimedOut, ctx.Err().Error()
	}

	res.Duration = time.Since(started)
	o.Journal.Append(journal.Record{
		Task: id, Event: journal.Finished, Pane: o.Pane,
		Outcome: res.Outcome, Reason: res.Reason, Attempt: o.Attempt,
	})
	return res, nil
}

// Report вызывается изнутри панели: свою панель процесс знает из окружения,
// которое herdr кладёт в каждую.
func Report(j *journal.Journal, id, outcome, reason string) error {
	return j.Append(journal.Record{
		Task: id, Event: journal.Reported, Pane: os.Getenv("HERDR_PANE_ID"),
		Outcome: outcome, Reason: reason,
	})
}

func withReportInstruction(prompt, id string) string {
	return prompt + fmt.Sprintf(
		"\n\nКогда закончишь, последним действием выполни:\n  claudex done %s «одной строкой, что вышло»", id)
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
