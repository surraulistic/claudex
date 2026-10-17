package task

import (
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

// Исходы продолжения, как они пишутся в журнал.
const (
	ContinuationSent   = "отправлено"
	ContinuationFailed = "не отправлено"
)

// Continuation — продолжение поручения: текст и чем кончилась отправка.
// Outcome пуст, если исход не записан (процесс оборвался посреди отправки).
type Continuation struct {
	Attempt int       `json:"attempt"`
	At      time.Time `json:"at"`
	Prompt  string    `json:"prompt"`
	Outcome string    `json:"outcome,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// BeginContinuation записывает текст продолжения до отправки: без записи
// его не видно ни в `task`, ни в `undelivered`, и отказ теряет текст.
func BeginContinuation(j *journal.Journal, id, executor, prompt string) (int, error) {
	recs, err := j.Read()
	if err != nil {
		return 0, err
	}
	n := len(Continuations(recs, id)) + 1
	return n, j.Append(journal.Record{Task: id, Event: journal.Continued, PaneSession: executor,
		Prompt: prompt, Attempt: n})
}

// EndContinuation дописывает исход отправки.
func EndContinuation(j *journal.Journal, id string, attempt int, sendErr error) error {
	r := journal.Record{Task: id, Event: journal.Continued, Attempt: attempt, Outcome: ContinuationSent}
	if sendErr != nil {
		r.Outcome, r.Reason = ContinuationFailed, sendErr.Error()
	}
	return j.Append(r)
}

// Continuations — продолжения поручения по порядку.
func Continuations(recs []journal.Record, id string) []Continuation {
	var out []Continuation
	at := map[int]int{}
	for _, r := range recs {
		if r.Task != id || r.Event != journal.Continued {
			continue
		}
		if r.Outcome == "" {
			at[r.Attempt] = len(out)
			out = append(out, Continuation{Attempt: r.Attempt, At: r.Time, Prompt: r.Prompt})
			continue
		}
		if i, ok := at[r.Attempt]; ok {
			out[i].Outcome, out[i].Reason = r.Outcome, r.Reason
		}
	}
	return out
}
