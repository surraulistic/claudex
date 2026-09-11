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
	// Unknown — herdr отказал во время ожидания. Это не «не завершилась»:
	// мы попросту не узнали, чем дело кончилось, и выдавать одно за другое
	// нельзя.
	Unknown = "исход неизвестен"
)

var ErrBusy = errors.New("панель занята")

// Free — принимает ли панель в таком состоянии новое задание. Проверка нужна
// снаружи тоже: у отправки без ожидания своя ветка, и она про это забывала.
func Free(status string) bool { return freeStates[status] }

// Состояния, в которых панель принимает новое задание.
//
// blocked сюда не входит: панель ждёт решения человека, и произвольный текст
// уедет ответом на этот вопрос — вплоть до подтверждения того, чего никто не
// подтверждал. unknown тоже: мы попросту не знаем, что там.
var freeStates = map[string]bool{"idle": true, "done": true}

type Options struct {
	Client  *herdr.Client
	Journal *journal.Journal
	Pane    string
	Prompt  string
	Timeout time.Duration
	// Grace — сколько ждать отчёт после того, как панель уже освободилась.
	Grace time.Duration
	// Self — чем отчитываться. По умолчанию — тот же бинарь, что делегирует:
	// на PATH может лежать другая сборка, которая про `done` не знает.
	Self string
	// Force отправляет задание панели, которая ждёт решения человека. Осознанно
	// и только по прямой просьбе.
	Force   bool
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
	if !freeStates[pane.Status] && !o.Force {
		return Result{}, fmt.Errorf("%w: %s в состоянии %q", ErrBusy, o.Pane, pane.Status)
	}

	if o.Self == "" {
		o.Self = selfPath()
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
		_, err := o.Client.Prompt(o.Pane, withReportInstruction(o.Prompt, o.Self, id),
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
			// Срок и отказ herdr — разные исходы для вызывающего.
			res.Outcome = Unknown
			if herdr.CodeOf(err) == "timeout" {
				res.Outcome = TimedOut
			}
			res.Reason = err.Error()
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

func withReportInstruction(prompt, self, id string) string {
	return prompt + fmt.Sprintf(
		"\n\nКогда закончишь, последним действием выполни ровно это:\n  %s done %s «одной строкой, что вышло»", self, id)
}

func selfPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "claudex"
}

// NewID — идентификатор поручения. Нужен и снаружи: отправка без ожидания
// заводит запись в журнале сама.
func NewID() string { return newID() }

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Notify будит ведущего после завершения поручения. Панель ведущего может быть
// занята своим ходом, поэтому попытки повторяются: разбудить с третьего раза
// лучше, чем не разбудить вовсе.
func Notify(ctx context.Context, c *herdr.Client, target, text string, attempts int, gap time.Duration) error {
	if attempts <= 0 {
		attempts = 5
	}
	if gap <= 0 {
		gap = 3 * time.Second
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(gap):
			}
		}
		pane, err := c.Get(target)
		if err != nil {
			last = err
			continue
		}
		if !freeStates[pane.Status] {
			last = fmt.Errorf("%w: %s в состоянии %q", ErrBusy, target, pane.Status)
			continue
		}
		if _, err := c.Prompt(target, text, nil, 0); err != nil {
			last = err
			continue
		}
		return nil
	}
	return fmt.Errorf("не удалось разбудить %s за %d попыток: %w", target, attempts, last)
}
