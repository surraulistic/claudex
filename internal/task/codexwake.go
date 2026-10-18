package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/journal"
)

// Исходы доставки в тред Codex. Принятое в очередь до ведущего ещё не дошло:
// дошло — только когда начался ход.
const (
	ThreadNotQueued   = "не положено в очередь треда, ход не начат"
	ThreadQueued      = "принято в очередь треда, ход не начат"
	ThreadTurnStarted = WokeUp + ": ход треда начат"
	ThreadTurnDone    = WokeUp + ": ход треда завершён"
	// ThreadTurnFailed — ход с событием оборвался: до ведущего не дошло, а
	// повтор мог бы повторить сделанное, поэтому дожим его не трогает.
	ThreadTurnFailed = NotDelivered + ": ход треда оборвался"
)

// CauseTurnFailed — ход с событием кончился failed или interrupted.
const CauseTurnFailed = codex.HoldTurnFailed

// CauseConsumedUnconfirmed — запись `codex queue` из очереди взята, а хода с
// её текстом не нашлось: прочитана ли она, неизвестно.
const CauseConsumedUnconfirmed = codex.HoldConsumedUnconfirmed

// NoRetry — повтор доставки с такой причиной бесполезен или опасен: ход мог
// повторить сделанное, а неподтверждённое повтор не подтвердит.
func NoRetry(cause string) bool {
	return cause == CauseTurnFailed || cause == CauseConsumedUnconfirmed
}

// ThreadClientID — постоянный идентификатор сообщения для пары
// поручение+стадия. По нему повтор находит уже положенное.
func ThreadClientID(task, stage string) string {
	return codex.ClientIDPrefix + task + "-" + orElse(stage, StageFinished)
}

// threadProgress — что журнал знает о доставке этой стадии в тред.
type threadProgress struct {
	stage    string
	hold     string
	thread   string
	turn     string
	clientID string
	// cliHash — TextHash своей записи, положенной `codex queue`.
	cliHash string
	// seen — какие пары стадия+причина уже записаны: мерцание причины
	// туда-обратно журнал не растит.
	seen  map[string]bool
	first time.Time
}

func (p threadProgress) accepted() bool { return rank(p.stage) >= rank(codex.StageAccepted) }

func progressOf(recs []journal.Record, id, stage string) threadProgress {
	p := threadProgress{seen: map[string]bool{}}
	for _, r := range recs {
		if r.Task != id || r.Event != journal.Notified || r.ThreadStage == "" ||
			orElse(r.Stage, StageFinished) != orElse(stage, StageFinished) {
			continue
		}
		p.seen[r.ThreadStage+"|"+r.Cause] = true
		if p.first.IsZero() {
			p.first = r.Time
		}
		if rank(r.ThreadStage) >= rank(p.stage) {
			p.stage, p.hold = r.ThreadStage, r.Cause
		}
		p.thread = r.Target
		if r.TurnID != "" {
			p.turn = r.TurnID
		}
		if r.ThreadClientID != "" {
			p.clientID = r.ThreadClientID
		}
		if r.Transport == codex.TransportQueueOnly && r.QueuedTextHash != "" {
			p.cliHash = r.QueuedTextHash
		}
	}
	return p
}

func rank(stage string) int {
	switch stage {
	case codex.StageAccepted:
		return 1
	case codex.StageStarted:
		return 2
	case codex.StageCompleted, codex.StageTurnFailed:
		return 3
	}
	return 0
}

func threadOutcome(stage string) string {
	switch stage {
	case codex.StageStarted:
		return ThreadTurnStarted
	case codex.StageCompleted:
		return ThreadTurnDone
	case codex.StageTurnFailed:
		return ThreadTurnFailed
	case codex.StageNotQueued:
		return ThreadNotQueued
	}
	return ThreadQueued
}

// recordThread пишет стадию, только если она сдвинулась или сменилась
// причина задержки: наблюдатель повторяет попытки каждый тик, и журнал не
// должен превращаться в их перечень.
func recordThread(j *journal.Journal, id, thread, stage, mode string, prev threadProgress,
	r codex.WakeResult) {
	if j == nil {
		return
	}
	// Смена прав открытой сессии пишется всегда: это то, что человек должен
	// увидеть.
	if r.ProfileSet != codex.ProfileByUpdate && (rank(r.Stage) < rank(prev.stage) ||
		(r.Stage == prev.stage && r.Hold == prev.hold) || prev.seen[r.Stage+"|"+r.Hold]) {
		return
	}
	// Начатый и сразу завершённый ход — две стадии, и обе видны.
	if rank(r.Stage) == rank(codex.StageCompleted) && rank(prev.stage) < rank(codex.StageStarted) {
		j.Append(journal.Record{Task: id, Event: journal.Notified, Target: thread,
			TargetKind: KindThread, Stage: stage, Delivery: mode, Outcome: ThreadTurnStarted,
			ThreadStage: codex.StageStarted, Transport: r.Transport, QueueID: r.QueueID, TurnID: r.TurnID,
			ThreadClientID: r.ClientID})
	}
	reason := r.Reason
	if r.Stage == codex.StageCompleted && r.TurnStatus != "" {
		reason = "статус хода: " + r.TurnStatus
	}
	if r.ProfileSet != "" {
		reason = strings.TrimSpace(reason + " · " + profileNote(r))
	}
	j.Append(journal.Record{Task: id, Event: journal.Notified, Target: thread,
		TargetKind: KindThread, Stage: stage, Delivery: mode, Outcome: threadOutcome(r.Stage),
		Cause: r.Hold, Reason: reason,
		ThreadStage: r.Stage, Transport: r.Transport, QueueID: r.QueueID, TurnID: r.TurnID,
		ThreadClientID: r.ClientID, PermissionsSet: r.ProfileSet, PermissionsFrom: r.ProfileFrom,
		QueuedTextHash: r.TextHash})
}

func profileNote(r codex.WakeResult) string {
	from := r.ProfileFrom
	if from == "" {
		from = "без именованного профиля"
	}
	switch r.ProfileSet {
	case codex.ProfileByUpdate:
		return "права треда переключены на " + codex.WakePermissionProfile + " (было: " + from + ")"
	case codex.ProfileByUpdateUnconfirmed:
		return "послан перевод прав треда на " + codex.WakePermissionProfile + " (было: " + from +
			"), применение не подтверждено"
	case codex.ProfileByResumeLoaded:
		return "загруженный тред поднят resume с правами " + codex.WakePermissionProfile +
			": без подписчиков сервер перезагружает его с ними, прежние права не названы"
	}
	return "тред поднят с правами " + codex.WakePermissionProfile
}

// threadHints — что журнал знает об адресе в целом, а не об одной стадии:
// клались ли туда сообщения `codex queue` и менял ли claudex права треда.
//
// Неподтверждённая смена считается сменой: права могли и смениться, а
// переключать их второй раз после того, как человек вернул свои, нельзя.
func threadHints(recs []journal.Record, thread string) (cli []codex.CLIQueued, switched bool) {
	for _, r := range recs {
		if r.Target != thread || r.Event != journal.Notified {
			continue
		}
		if r.PermissionsSet != "" {
			switched = true
		}
		if r.Transport == codex.TransportQueueOnly && r.ThreadStage == codex.StageAccepted {
			cli = append(cli, codex.CLIQueued{Task: r.Task, Hash: r.QueuedTextHash})
		}
	}
	return cli, switched
}

// FollowedTurn — ход, чьё завершение замечено.
type FollowedTurn struct {
	Task   string `json:"task"`
	Thread string `json:"thread"`
	Turn   string `json:"turn"`
	Status string `json:"status"`
}

// FollowTurns дописывает в журнал завершение ходов, начатых пробуждением.
// Только читает app-server: ничего не кладёт и не запускает.
func FollowTurns(ctx context.Context, j *journal.Journal, max int) []FollowedTurn {
	if j == nil {
		return nil
	}
	recs, err := j.Read()
	if err != nil {
		return nil
	}
	type key struct{ task, stage string }
	open := map[key]journal.Record{}
	var order []key
	for _, r := range recs {
		if r.Event != journal.Notified || r.ThreadStage == "" {
			continue
		}
		k := key{r.Task, orElse(r.Stage, StageFinished)}
		switch r.ThreadStage {
		case codex.StageStarted:
			if _, ok := open[k]; !ok {
				order = append(order, k)
			}
			open[k] = r
		case codex.StageCompleted, codex.StageTurnFailed:
			delete(open, k)
		}
	}
	var out []FollowedTurn
	for _, k := range order {
		r, ok := open[k]
		if !ok || ctx.Err() != nil || len(out) >= max {
			continue
		}
		if time.Since(r.Time) > AbandonAfter {
			continue
		}
		res, err := codex.Follow(ctx, r.Target, orElse(r.ThreadClientID, ThreadClientID(k.task, k.stage)),
			r.Time, 5*time.Second)
		if err != nil || rank(res.Stage) != rank(codex.StageCompleted) {
			continue
		}
		recordThread(j, k.task, r.Target, k.stage, r.Delivery,
			threadProgress{stage: codex.StageStarted}, res)
		out = append(out, FollowedTurn{Task: k.task, Thread: r.Target, Turn: res.TurnID, Status: res.TurnStatus})
	}
	return out
}

// ThreadTimeline — стадии доставки в тред Codex, строкой на запись.
func ThreadTimeline(recs []journal.Record, id string) []string {
	var out []string
	for _, r := range recs {
		if r.Task != id || r.Event != journal.Notified || r.ThreadStage == "" {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s  %s", r.Time.Format("02.01 15:04:05"), stageWords(r.ThreadStage))
		if r.Transport == codex.TransportQueueOnly {
			b.WriteString(" (codex queue, без app-server)")
		}
		if r.TurnID != "" {
			fmt.Fprintf(&b, " · ход %s", short(r.TurnID))
		}
		if r.Reason != "" {
			fmt.Fprintf(&b, " — %s", r.Reason)
		}
		out = append(out, b.String())
	}
	return out
}

func stageWords(stage string) string {
	switch stage {
	case codex.StageNotQueued:
		return "not queued: не положено в очередь, ход не начат"
	case codex.StageAccepted:
		return "queue accepted: принято в очередь, ход не начат"
	case codex.StageStarted:
		return "turn started: ход треда начат"
	case codex.StageCompleted:
		return "turn completed: ход треда завершён"
	case codex.StageTurnFailed:
		return "turn failed: ход треда оборвался, событие могло остаться необработанным"
	}
	return stage
}

// wakeThread повторяет пробуждение при преходящем отказе: демон app-server
// перезапускается, и следующая попытка через пару секунд проходит. Повтор
// безопасен — своё сообщение находится по ThreadClientID.
func wakeThread(ctx context.Context, req codex.WakeRequest, o DeliverOptions) (codex.WakeResult, error) {
	attempts, gap := o.QueueAttempts, o.QueueGap
	if attempts <= 0 {
		attempts = 3
	}
	if gap <= 0 {
		gap = 2 * time.Second
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return codex.WakeResult{}, fmt.Errorf("%w (после %d попыток: %v)", ctx.Err(), i, last)
			case <-time.After(gap):
			}
		}
		r, err := codex.Wake(ctx, req)
		if err == nil {
			return r, nil
		}
		last = err
	}
	return codex.WakeResult{}, last
}
