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
	"strings"
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
	Force bool
	// Notify — панель, которую будить по завершении. Пусто значит «никого»;
	// поздний отчёт всё равно уйдёт тому, кто поручение затеял.
	Notify  string
	Attempt int
}

type Result struct {
	// WakeTarget и WakeSession — кого будить и какой разговор там должен быть.
	// Снимаются в момент заведения поручения, а не доставки.
	WakeTarget  string
	WakeSession string
	Task        string
	Outcome     string // наша классификация: отчиталась / без отчёта / не уложилась
	Said        string // как сама задача назвала исход
	Reason      string
	Pane        string
	Duration    time.Duration
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
	// Будить будем именно тот разговор, который поручение затеял: панель
	// переживает смену агента, а разговор в ней — нет.
	wake := orElse(o.Notify, os.Getenv("HERDR_PANE_ID"))
	wakeSession := sessionOf(o.Client, wake)

	id := newID()
	started := time.Now()
	res := Result{Task: id, Pane: o.Pane, WakeTarget: wake, WakeSession: wakeSession}
	o.Journal.Append(journal.Record{
		Task: id, Event: journal.Started, Pane: o.Pane,
		Target: wake, TargetSession: wakeSession,
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
		DeliverOptions{Stage: StageReported, WantSession: st.session,
			Deadline: o.Deadline, Poll: o.Poll})
	res.Delivered = &d
	return res, nil
}

// Lost — завершение, о котором не узнал никто: ни ведущий, ни человек.
// Отдаётся в выдаче sessions и brief, потому что её и без того постоянно
// запрашивают: это канал, который не зависит от того, показал ли herdr
// уведомление.
type Lost struct {
	Task   string `json:"task"`
	Stage  string `json:"stage"`
	Target string `json:"target"`
	At     string `json:"at"`
	Reason string `json:"reason"`
	Report string `json:"report,omitempty"`
}

// LostReports собирает по журналу всё, что осталось никем не полученным.
func LostReports(recs []journal.Record) []Lost {
	type key struct{ task, stage string }
	state := map[key]*Lost{}
	var order []key
	report := map[string]string{}

	for _, r := range recs {
		switch r.Event {
		case journal.Reported:
			report[r.Task] = strings.TrimSpace(r.Outcome + " " + r.Reason)
		case journal.Notified:
			k := key{r.Task, orElse(r.Stage, StageFinished)}
			if _, ok := state[k]; !ok {
				order = append(order, k)
			}
			if !strings.HasPrefix(r.Outcome, NotDelivered) {
				state[k] = nil // до кого-то дошло
				continue
			}
			state[k] = &Lost{
				Task: r.Task, Stage: k.stage, Target: r.Target,
				At: r.Time.Format(time.RFC3339), Reason: r.Outcome + ": " + r.Reason,
			}
		}
	}
	out := []Lost{}
	for _, k := range order {
		if l := state[k]; l != nil {
			l.Report = report[l.Task]
			out = append(out, *l)
		}
	}
	return out
}

type taskState struct {
	finished  bool
	target    string
	session   string
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
				st.target, st.session = r.Target, r.TargetSession
			}
		case journal.Finished:
			st.finished = true
		case journal.Notified:
			if r.Target != "" {
				st.target = r.Target
			}
			if !strings.HasPrefix(r.Outcome, NotDelivered) {
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
	// Refused — почему herdr отказался показывать. Пусто при удачном показе.
	Refused string `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Исходы доставки, как они пишутся в журнал. Признак «не доставлено» может
// нести причину отказа herdr, поэтому сверяется по началу строки.
const (
	WokeUp       = "разбужен"
	ToldHuman    = "не разбужен, показано человеку"
	NotDelivered = "не доставлено"
)

const (
	StageFinished = "finished" // исход, известный наблюдателю, в том числе по сроку
	StageReported = "reported" // настоящий отчёт задачи, пришедший позже
)

// sessionOf — какой разговор сейчас живёт в панели. Пусто значит, что герой
// его не сообщает: тогда привязки нет и проверять нечего.
func sessionOf(c *herdr.Client, pane string) string {
	if c == nil || pane == "" {
		return ""
	}
	a, err := c.Get(pane)
	if err != nil {
		return ""
	}
	return a.Session.Value
}

type DeliverOptions struct {
	// WantSession — разговор, который поручение затеял. Писать в панель можно
	// только когда там ровно он: рядом живут другие сессии того же ведущего,
	// и попасть в чужую нельзя ни при каких обстоятельствах.
	WantSession string
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
		case !sameConversation(o.WantSession, pane.Session.Value):
			d.Waited = time.Since(started)
			d.Seconds = int(d.Waited.Seconds())
			d.Reason = mismatch(target, o.WantSession, pane.Session.Value)
			d.Fallback, d.Refused = tellHuman(c, "claudex: поручение "+id+" завершено",
				text+"\n\n"+d.Reason)
			record(j, id, target, o.Stage, d)
			return d
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
			d.Fallback, d.Refused = tellHuman(c, "claudex: поручение "+id+" завершено",
				text+"\n\nРазбудить "+target+" не удалось: "+last.Error())
			record(j, id, target, o.Stage, d)
			return d
		case <-time.After(o.Poll):
		}
	}
}

// Резать по символам, а не по байтам: у кириллического имени разговора
// байтовый срез рубит букву пополам, и диагностика становится бесполезной.
// tellHuman показывает исход человеку и не выдаёт отказ за успех.
//
// Отказ herdr бывает преходящим (наблюдалось busy, через десять секунд то же
// уведомление показывалось), поэтому несколько попыток. Долго ждать нельзя:
// эта ветка выполняется в том числе внутри хода самой задачи.
var humanRetryGap = 3 * time.Second

func tellHuman(c *herdr.Client, title, body string) (bool, string) {
	var reason string
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(humanRetryGap)
		}
		shown, why, err := c.Notify(title, body)
		if shown {
			return true, ""
		}
		reason = why
		if err != nil {
			reason = err.Error()
		}
	}
	return false, reason
}

// sameConversation — тот ли это разговор, что затеял поручение.
//
// Запрещено, пока не доказано. Неизвестный с любой стороны идентификатор —
// это не «наверное, тот же», а «подтвердить нечем»: у панели бывает пусто
// в agent_session, и прежнее мягкое правило писало в неё что угодно.
func sameConversation(want, have string) bool {
	return want != "" && have != "" && want == have
}

func mismatch(target, want, have string) string {
	switch {
	case want == "":
		return fmt.Sprintf("при заведении поручения разговор в %s не был записан: "+
			"подтвердить, что это он, нечем", target)
	case have == "":
		return fmt.Sprintf("herdr не сообщает, какой разговор сейчас в %s: "+
			"подтвердить, что это %s, нечем", target, short(want))
	default:
		return fmt.Sprintf("в %s теперь другой разговор (%s вместо %s): поручение затевал не он",
			target, short(have), short(want))
	}
}

func short(id string) string {
	r := []rune(id)
	if len(r) > 8 {
		return string(r[:8]) + "…"
	}
	return id
}

func record(j *journal.Journal, id, target, stage string, d Delivery) {
	if j == nil {
		return
	}
	outcome := WokeUp
	switch {
	case d.OK:
	case d.Fallback:
		outcome = ToldHuman
	default:
		outcome = NotDelivered
		if d.Refused != "" {
			outcome += " (herdr: " + d.Refused + ")"
		}
	}
	j.Append(journal.Record{
		Task: id, Event: journal.Notified, Target: target, Stage: stage,
		Outcome: outcome, Reason: d.Reason,
	})
}
