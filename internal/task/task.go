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
type ReportOptions struct {
	Client  *herdr.Client
	Journal *journal.Journal
	// Deadline — сколько ждать освобождения ведущего. Здесь он короткий, в
	// отличие от наблюдателя: done выполняется внутри хода самой задачи, и
	// держать её минутами нельзя. Не дождались — говорим человеку сразу.
	Deadline time.Duration
	Poll     time.Duration
}

type ReportResult struct {
	Task      string    `json:"task"`
	Late      bool      `json:"late"`
	Delivered *Delivery `json:"delivered,omitempty"`
	Skipped   string    `json:"skipped,omitempty"`
}

// Report записывает отчёт задачи и, если её уже похоронили, доставляет его сам.
//
// Наблюдатель живёт ровно до своего срока. Задача, не уложившаяся в него,
// продолжает работать и отчитывается позже — этот отчёт прежде не доходил
// никуда: наблюдателя уже нет, а done только дописывал строку в журнал.
// Срок наблюдателя не делает задачу законченной.
func Report(ctx context.Context, id, outcome, reason string, o ReportOptions) (ReportResult, error) {
	res := ReportResult{Task: id}
	if o.Journal == nil {
		return res, fmt.Errorf("журнал не задан")
	}
	before, _ := o.Journal.Read()

	if err := o.Journal.Append(journal.Record{
		Task: id, Event: journal.Reported, Pane: os.Getenv("HERDR_PANE_ID"),
		Outcome: outcome, Reason: reason,
	}); err != nil {
		return res, err
	}

	st := stateOf(before, id)
	switch {
	case !st.finished:
		res.Skipped = "наблюдатель ещё ждёт — он и доставит"
		return res, nil
	case st.delivered[StageReported]:
		res.Skipped = "поздний отчёт уже доставлен"
		return res, nil
	case st.target == "":
		res.Skipped = "ведущий не назначался, доставлять некому"
		return res, nil
	}

	res.Late = true
	if o.Client == nil {
		res.Skipped = "herdr недоступен"
		return res, nil
	}
	if o.Deadline <= 0 {
		o.Deadline = 15 * time.Second
	}
	d := Deliver(ctx, o.Client, o.Journal, id, st.target,
		fmt.Sprintf("Поручение %s завершилось после срока наблюдения: %s %s", id, outcome, reason),
		DeliverOptions{Stage: StageReported, Deadline: o.Deadline, Poll: o.Poll})
	res.Delivered = &d
	return res, nil
}

type taskState struct {
	finished  bool
	target    string
	delivered map[string]bool
}

// stateOf собирает по журналу то, что нужно решить о поздней доставке.
// Стадия считается доставленной, только если кого-то действительно достигли:
// провалившуюся попытку повторить стоит, удавшуюся — нет.
func stateOf(recs []journal.Record, id string) taskState {
	st := taskState{delivered: map[string]bool{}}
	for _, r := range recs {
		if r.Task != id {
			continue
		}
		switch r.Event {
		case journal.Started:
			if r.Target != "" {
				st.target = r.Target
			}
		case journal.Finished:
			st.finished = true
		case journal.Notified:
			if r.Target != "" {
				st.target = r.Target
			}
			if r.Outcome != "не доставлено" {
				st.delivered[orElse(r.Stage, StageFinished)] = true
			}
		}
	}
	return st
}

func orElse(v, def string) string {
	if v == "" {
		return def
	}
	return v
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

// Delivery — чем кончилась доставка факта завершения.
type Delivery struct {
	Target   string        `json:"target"`
	OK       bool          `json:"ok"`
	Waited   time.Duration `json:"-"`
	Seconds  int           `json:"waited_seconds"`
	Fallback bool          `json:"fallback,omitempty"`
	Reason   string        `json:"reason,omitempty"`
}

const (
	StageFinished = "finished" // исход, известный наблюдателю, в том числе по сроку
	StageReported = "reported" // настоящий отчёт задачи, пришедший позже
)

type DeliverOptions struct {
	// Stage — что именно доставляется. Дедупликация ведётся по паре
	// задача+стадия: повторный done не должен будить ведущего второй раз.
	Stage string
	// Deadline — сколько ждать, пока ведущий освободится. Ожидание живёт в уже
	// запущенном наблюдателе и стоит только опроса herdr: токенов оно не тратит,
	// поэтому пятнадцати секунд здесь было мало на порядок.
	Deadline time.Duration
	Poll     time.Duration
}

// Deliver доносит до ведущего, что поручение закончилось, и не сдаётся молча.
//
// Сначала ждёт, пока панель ведущего освободится: писать в занятую нельзя —
// текст уедет в чужой ход. Если за отведённое время она так и не освободилась,
// факт уходит человеку уведомлением herdr — канал, который не зависит ни от
// одного агента. Итог в обоих случаях попадает в журнал: недоставленное
// пробуждение должно быть видно, а не лежать в логе, который никто не читает.
func Deliver(ctx context.Context, c *herdr.Client, j *journal.Journal,
	id, target, text string, o DeliverOptions) Delivery {

	if o.Deadline <= 0 {
		o.Deadline = 30 * time.Minute
	}
	if o.Poll <= 0 {
		o.Poll = 5 * time.Second
	}
	started := time.Now()
	d := Delivery{Target: target}

	ctx, cancel := context.WithTimeout(ctx, o.Deadline)
	defer cancel()
	var last error
	for {
		pane, err := c.Get(target)
		switch {
		case err != nil:
			last = err
		case !freeStates[pane.Status]:
			last = fmt.Errorf("%s в состоянии %q", target, pane.Status)
		default:
			if _, err := c.Prompt(target, text, nil, 0); err == nil {
				d.OK = true
				d.Waited = time.Since(started)
				d.Seconds = int(d.Waited.Seconds())
				record(j, id, target, o.Stage, d)
				return d
			} else {
				last = err
			}
		}
		select {
		case <-ctx.Done():
			d.Waited = time.Since(started)
			d.Seconds = int(d.Waited.Seconds())
			d.Reason = fmt.Sprintf("ведущий не освободился за %s: %v", o.Deadline, last)
			d.Fallback = c.Notify("claudex: поручение "+id+" завершено",
				text+"\n\nРазбудить "+target+" не удалось: "+last.Error()) == nil
			record(j, id, target, o.Stage, d)
			return d
		case <-time.After(o.Poll):
		}
	}
}

func record(j *journal.Journal, id, target, stage string, d Delivery) {
	if j == nil {
		return
	}
	outcome := "разбужен"
	switch {
	case d.OK:
	case d.Fallback:
		outcome = "не разбужен, показано человеку"
	default:
		outcome = "не доставлено"
	}
	j.Append(journal.Record{
		Task: id, Event: journal.Notified, Target: target, Stage: stage,
		Outcome: outcome, Reason: d.Reason,
	})
}
