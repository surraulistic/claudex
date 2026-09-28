package task

import (
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

// Молчание после окончания хода.
//
// Хук Stop пишет в журнал, что исполнитель доработал ход и что он сказал
// последней репликой. Одно это завершением не является: ход кончается и
// посреди работы, и когда задача задаёт вопрос. Завершением его делает время —
// исполнитель молчит и больше не начинает.
//
// Отсюда наблюдение, а не суждение. Разбудить затеявшего можно и нужно, но
// сказать ему «готово» нельзя: это оценка, а её даёт только `claudex done`.
// Событие говорит ровно то, что видно: работа кончилась, вот последняя
// реплика, подробности читай сам.

// SilenceGrace — сколько молчать, прежде чем молчание считать окончательным.
// Задача может вызвать done через минуту после последней реплики, и
// перехватывать её незачем: собственный отчёт задачи ценнее наблюдения.
const SilenceGrace = 5 * time.Minute

// Silence — поручение, исполнитель которого доработал и замолчал.
type Silence struct {
	Task string `json:"task"`
	// Last — последняя реплика исполнителя, как её передал хук. Может быть
	// пустой: тогда доказательства нет, и событие скажет только про молчание.
	Last string    `json:"last,omitempty"`
	Idle time.Time `json:"idle"`
}

// SilentTasks — поручения, по которым исполнитель закончил ход и с тех пор
// молчит дольше срока, а отчёта так и не дал.
func SilentTasks(recs []journal.Record, now time.Time, grace time.Duration) []Silence {
	if grace <= 0 {
		grace = SilenceGrace
	}
	reported := map[string]bool{}
	started := map[string]bool{}
	woken := map[string]bool{}
	idle := map[string]journal.Record{}
	for _, r := range recs {
		switch r.Event {
		case journal.Started:
			started[r.Task] = true
		case journal.Reported:
			reported[r.Task] = true
		case journal.Notified:
			// Затеявшего уже разбудили по этому поручению — второй раз за то
			// же будить незачем: дедупликация здесь та же, что у отчётов, по
			// паре задача+стадия.
			if r.Stage == StageReported && r.Outcome == WokeUp {
				woken[r.Task] = true
			}
		case journal.Idle:
			// Последняя отметка вытесняет прежние: ходов у задачи много, а
			// важен тот, после которого она замолчала.
			idle[r.Task] = r
		}
	}
	var out []Silence
	for id, r := range idle {
		if !started[id] || reported[id] || woken[id] {
			continue
		}
		if now.Sub(r.Time) < grace {
			continue
		}
		out = append(out, Silence{Task: id, Last: strings.TrimSpace(r.Reason), Idle: r.Time})
	}
	return out
}

// SilenceEvent — чем будить за молчуна.
//
// Состояние называется lost, а не done: мы видели, что работа кончилась, но
// чем она кончилась — не знаем. Подменять наблюдение оценкой значит соврать
// ровно в том месте, ради которого отчёт и нужен.
func SilenceEvent(s Silence, executor string) *Event {
	reason := "исполнитель закончил и не отчитался"
	if s.Last != "" {
		reason = "закончил молча; последняя реплика: " + s.Last
	}
	return &Event{
		Task: s.Task, State: StateLost, Stage: StageReported,
		Executor: executor, Reason: reason,
	}
}
