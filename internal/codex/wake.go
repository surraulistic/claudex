package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Пробуждение треда через уже работающий app-server.
//
// `codex queue` только сохраняет сообщение: у выгруженного треда ход не
// начинается, пока его кто-нибудь не откроет (openai/codex#44491, так задумано).
// Измерено на отдельном тестовом треде (0.162.1): загруженный простаивающий тред
// забирает очередь сам; выгруженный — после thread/resume;
// ход переживает отключение того, кто его разбудил.
//
// Стадии разные и пишутся порознь: «принято в очередь» не значит «прочитано».

const (
	// StageNotQueued — сообщение не положено: тред занят или профиль не
	// подтверждён. Повтор кладёт его заново.
	StageNotQueued = "not_queued"
	StageAccepted  = "queue_accepted"
	StageStarted   = "turn_started"
	StageCompleted = "turn_completed"
)

const (
	TransportAppServer = "app_server"
	TransportQueueOnly = "queue_only"
)

// WakePermissionProfile restricts woken Codex threads (cold-resumed or loaded
// idle) to the user's read-mostly profile instead of inheriting
// danger-full-access defaults.
const WakePermissionProfile = "claudex-work"

// Почему ход не начат — машиночитаемо.
const (
	HoldAppServerDown = "app_server_unavailable"
	HoldBusy          = "thread_busy"
	HoldWaitingHuman  = "thread_waiting_on_human"
	HoldSystemError   = "thread_system_error"
	HoldForeignQueue  = "foreign_queue_items"
	HoldOpenElsewhere = "thread_open_elsewhere"
	HoldEphemeral     = "thread_ephemeral"
	HoldItemGone      = "queue_item_gone"
	HoldNotStarted    = "turn_not_started"
	// Загруженный тред: профиль не прочитан resume или его смена не
	// подтверждена уведомлением сервера. Ничего не кладём.
	HoldLoadedProfile = "loaded_thread_profile_unconfirmed"
	// Профиль claudex-work не найден или не разрешён: resume не делался.
	HoldNoProfile = "permission_profile_unavailable"
	// Сервер после resume или thread/settings/update назвал не тот профиль
	// или небезопасный sandbox. Если своё сообщение уже лежало в очереди
	// выгруженного треда, ход с ним мог начаться.
	HoldWrongProfile = "permission_profile_mismatch"
)

// ClientIDPrefix метит сообщения claudex в очереди: по нему отличаем свои от
// того, что человек поставил сам.
const ClientIDPrefix = "claudex-"

// TextPrefix — с чего начинается любое событие claudex. Нужен для записей,
// положенных `codex queue`: своего clientUserMessageId у них нет.
const TextPrefix = "claudex: поручение "

type WakeRequest struct {
	Thread string
	// ClientID — постоянный для пары поручение+стадия: повтор находит своё
	// сообщение, а не кладёт второе.
	ClientID string
	// Task — поручение; по нему узнаются свои записи без ClientID.
	Task string
	// SeenClientID — фактический clientUserMessageId своей записи, если он
	// иной: у положенного `codex queue` он случайный.
	SeenClientID string
	Text         string
	// Accepted — журнал уже знает, что сообщение принято. Тогда класть его
	// снова нельзя ни при каких обстоятельствах, даже если в очереди его нет:
	// значит, его забрали или удалили.
	Accepted bool
	// OpenElsewhere — держит ли тред писатель вне демона. Вызывается только
	// для выгруженного треда.
	OpenElsewhere func() bool
	StartWait     time.Duration
	Poll          time.Duration
	// Each — предел на один вызов app-server.
	Each time.Duration
}

type WakeResult struct {
	Stage     string
	Transport string
	QueueID   string
	// ClientID — по какому clientId опознаётся ход с нашим сообщением.
	ClientID string
	TurnID   string
	// TurnStatus — статус хода, если он найден.
	TurnStatus string
	Hold       string
	Reason     string
}

// Started — дошло ли до хода.
func (r WakeResult) Started() bool { return r.Stage == StageStarted || r.Stage == StageCompleted }

// Wake кладёт событие в очередь треда только после того, как тред переведён на
// claudex-work, и запускает ход. Отказ по правилам — Hold с причиной, а не
// ошибка; если до queue/add не дошло, стадия StageNotQueued.
func Wake(ctx context.Context, req WakeRequest) (WakeResult, error) {
	defaults(&req)
	s, err := dialWithin(ctx, req.Each)
	if err != nil {
		return queueOnly(ctx, req, err)
	}
	defer s.Close()
	return wake(ctx, s, req)
}

// Follow смотрит, чем кончился ход, ничего не кладя и не запуская.
func Follow(ctx context.Context, thread, clientID string, each time.Duration) (WakeResult, error) {
	if each <= 0 {
		each = 10 * time.Second
	}
	s, err := dialWithin(ctx, each)
	if err != nil {
		return WakeResult{}, err
	}
	defer s.Close()
	c, cancel := context.WithTimeout(ctx, each)
	defer cancel()
	t, id, err := findTurn(c, s, thread, clientID)
	if err != nil || id == "" {
		return WakeResult{Transport: TransportAppServer}, err
	}
	return turnResult(t, id), nil
}

func defaults(r *WakeRequest) {
	if r.StartWait <= 0 {
		r.StartWait = 10 * time.Second
	}
	if r.Poll <= 0 {
		r.Poll = time.Second
	}
	if r.Each <= 0 {
		r.Each = 10 * time.Second
	}
	if r.OpenElsewhere == nil {
		r.OpenElsewhere = func() bool { return false }
	}
}

func dialWithin(ctx context.Context, d time.Duration) (AppServer, error) {
	c, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return Dial(c)
}

// queueOnly — app-server недоступен. Принять сообщение ещё можно, запустить
// ход — нет, и это говорится прямо.
func queueOnly(ctx context.Context, req WakeRequest, dialErr error) (WakeResult, error) {
	r := WakeResult{Stage: StageAccepted, Transport: TransportQueueOnly, Hold: HoldAppServerDown}
	if req.Accepted {
		r.Reason = "app-server недоступен; сообщение уже в очереди, ход не запускался: " + dialErr.Error()
		return r, nil
	}
	c, cancel := context.WithTimeout(ctx, req.Each)
	defer cancel()
	if err := Queue(c, req.Thread, req.Text); err != nil {
		return WakeResult{Transport: TransportQueueOnly},
			fmt.Errorf("app-server недоступен (%v), и codex queue не принял: %w", dialErr, err)
	}
	r.Reason = "принято codex queue; app-server недоступен, ход не запускался: " + dialErr.Error()
	return r, nil
}

func wake(ctx context.Context, s AppServer, req WakeRequest) (WakeResult, error) {
	res := WakeResult{Transport: TransportAppServer}
	call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, req.Each) }

	c, cancel := call()
	th, err := s.ReadThread(c, req.Thread)
	cancel()
	if err != nil {
		return res, err
	}
	if th.ID != "" && th.ID != req.Thread {
		return res, fmt.Errorf("app-server вернул тред %s вместо %s", th.ID, req.Thread)
	}
	if th.Ephemeral {
		return res, errors.New("тред эфемерный: очередь у него не переживает выгрузку")
	}

	// Ход с нашим сообщением уже был — повтор ничего не кладёт.
	c, cancel = call()
	t, id, err := findTurn(c, s, req.Thread, req.ClientID, req.SeenClientID)
	cancel()
	if err != nil {
		return res, err
	}
	if id != "" {
		return turnResult(t, id), nil
	}

	c, cancel = call()
	queue, err := s.QueueList(c, req.Thread)
	cancel()
	if err != nil {
		return res, err
	}
	mine, queued := own(queue, req)
	switch {
	case queued:
		res.Stage, res.QueueID, res.ClientID = StageAccepted, mine.ID, mine.ClientID
	case req.Accepted:
		res.Stage, res.Hold = StageAccepted, HoldItemGone
		res.Reason = "сообщение было принято, но в очереди его нет и хода с ним не видно: " +
			"его забрали или удалили; повторно не кладём"
		return res, nil
	default:
		res.Stage = StageNotQueued
	}

	// Всё, что может отказать, — до queue/add: положенное сообщение
	// вычитает любой, кто откроет тред, с его правами, а не с нашими.
	if hold, why := statusHold(th.Status); hold != "" {
		return held(res, hold, why), nil
	}
	loaded := th.Status.Type == StatusIdle
	// И resume, и загруженный тред вычитывают всю очередь, поэтому чужие
	// сообщения в ней — запрет: запускать их не нам.
	if foreign(queue) {
		return held(res, HoldForeignQueue,
			"в очереди треда есть сообщения не от claudex: тред запустил бы и их"), nil
	}
	if !loaded && req.OpenElsewhere() {
		return held(res, HoldOpenElsewhere,
			"тред выгружен в демоне, но его держит другой процесс: второй писатель недопустим"), nil
	}
	c, cancel = call()
	profiles, err := s.PermissionProfiles(c, th.Cwd)
	cancel()
	if why := profileUnusable(profiles, err); why != "" {
		return held(res, HoldNoProfile, why+": права треда не меняем"), nil
	}
	if loaded {
		if r, ok := confirmLoaded(ctx, s, req, res); !ok {
			return r, nil
		}
	} else {
		c, cancel = call()
		rs, err := s.Resume(c, req.Thread, WakePermissionProfile)
		cancel()
		if err != nil {
			return held(res, HoldNotStarted, "thread/resume не прошёл: "+err.Error()), nil
		}
		if rs.Profile != WakePermissionProfile || rs.Sandbox == "dangerFullAccess" {
			return held(res, HoldWrongProfile, fmt.Sprintf(
				"после resume профиль %q, sandbox %q вместо %s: ничего не кладём и не запускаем",
				rs.Profile, rs.Sandbox, WakePermissionProfile)), nil
		}
	}

	if !queued {
		// Между первым чтением и сменой профиля тред мог занять кто-то другой.
		c, cancel = call()
		now, err := s.ReadThread(c, req.Thread)
		cancel()
		if err != nil {
			return held(res, HoldNotStarted, "после смены профиля тред не прочитан: "+err.Error()), nil
		}
		if now.Status.Type != StatusIdle {
			hold, why := statusHold(now.Status)
			if hold == "" {
				hold, why = HoldNotStarted, fmt.Sprintf("после смены профиля тред в состоянии %q", now.Status.Type)
			}
			return held(res, hold, why), nil
		}
		c, cancel = call()
		mine, err = s.QueueAdd(c, req.Thread, req.ClientID, req.Text)
		cancel()
		if err != nil {
			return res, err
		}
		res.Stage, res.QueueID, res.ClientID = StageAccepted, mine.ID, mine.ClientID
	}

	res, done := pollStart(ctx, s, req, res)
	if done {
		return res, nil
	}
	if loaded {
		// Загруженный тред не вычитал очередь сам — значит, её держит его
		// клиент; запускать в обход него не нам.
		return held(res, HoldNotStarted,
			"загруженный тред не взял сообщение из очереди: явно не запускаем"), nil
	}
	// resume сам очередь не вычитал. Запускаем ровно своё сообщение и только
	// если оно первое, а тред свободен.
	c, cancel = call()
	th, err = s.ReadThread(c, req.Thread)
	cancel()
	if err != nil || th.Status.Type != StatusIdle {
		return held(res, HoldNotStarted, "после resume тред не свободен: ход не запускаем"), nil
	}
	c, cancel = call()
	queue, err = s.QueueList(c, req.Thread)
	cancel()
	if err != nil || len(queue) == 0 || queue[0].ID != res.QueueID {
		return held(res, HoldNotStarted, "после resume наше сообщение не первое в очереди: ход не запускаем"), nil
	}
	c, cancel = call()
	turn, err := s.QueueStart(c, req.Thread, res.QueueID)
	cancel()
	if err != nil {
		return held(res, HoldNotStarted, "thread/queue/start не прошёл: "+err.Error()), nil
	}
	res.Stage, res.TurnID, res.TurnStatus, res.Hold, res.Reason = StageStarted, turn.ID, turn.Status, "", ""
	if turn.Status != "" && turn.Status != TurnInProgress {
		res.Stage = StageCompleted
	}
	return res, nil
}

// confirmLoaded переводит загруженный тред на claudex-work. thread/settings/updated
// сервер шлёт только подписчикам треда и только при фактической смене, поэтому
// сначала resume: он подписывает соединение и называет действующий профиль.
func confirmLoaded(ctx context.Context, s AppServer, req WakeRequest, res WakeResult) (WakeResult, bool) {
	c, cancel := context.WithTimeout(ctx, req.Each)
	rs, err := s.Resume(c, req.Thread, WakePermissionProfile)
	cancel()
	if err != nil {
		return held(res, HoldLoadedProfile,
			"профиль загруженного треда не прочитан (thread/resume: "+err.Error()+"): ничего не кладём и не запускаем"), false
	}
	if rs.Profile == WakePermissionProfile {
		if !safeSandbox(rs.Sandbox) {
			return held(res, HoldWrongProfile, fmt.Sprintf(
				"профиль %s, но sandbox %q: ничего не кладём и не запускаем", WakePermissionProfile, rs.Sandbox)), false
		}
		return res, true
	}
	c, cancel = context.WithTimeout(ctx, req.Each)
	st, err := s.SetPermissions(c, req.Thread, WakePermissionProfile)
	cancel()
	if err != nil {
		return held(res, HoldLoadedProfile,
			"профиль загруженного треда не подтверждён ("+err.Error()+"): ничего не кладём и не запускаем"), false
	}
	if st.Profile != WakePermissionProfile || !safeSandbox(st.Sandbox) {
		return held(res, HoldWrongProfile, fmt.Sprintf(
			"после thread/settings/update профиль %q, sandbox %q вместо %s: ничего не кладём и не запускаем",
			st.Profile, st.Sandbox, WakePermissionProfile)), false
	}
	return res, true
}

// safeSandbox — только песочницы самого Codex: externalSandbox, как и
// dangerFullAccess, даёт полный доступ к диску.
func safeSandbox(t string) bool { return t == "readOnly" || t == "workspaceWrite" }

// statusHold — почему тред с таким состоянием нельзя трогать. Пусто — можно.
func statusHold(st ThreadStatus) (string, string) {
	switch st.Type {
	case StatusNotLoaded, StatusIdle:
		return "", ""
	case StatusActive:
		if st.WaitingOnHuman() {
			return HoldWaitingHuman, "тред ждёт решения человека: не будим, повторим позже"
		}
		return HoldBusy, "тред занят ходом: повторим, когда освободится"
	case StatusSystemError:
		return HoldSystemError, "тред в состоянии systemError: не будим"
	}
	return HoldNotStarted, fmt.Sprintf("неизвестное состояние треда %q: не будим", st.Type)
}

// pollStart ждёт ход со своим сообщением. Опрос, а не подписка: подписка
// сделала бы claudex клиентом треда, а ему нужен только ответ «начался ли».
func pollStart(ctx context.Context, s AppServer, req WakeRequest, res WakeResult) (WakeResult, bool) {
	deadline := time.Now().Add(req.StartWait)
	for {
		c, cancel := context.WithTimeout(ctx, req.Each)
		t, id, err := findTurn(c, s, req.Thread, req.ClientID, res.ClientID)
		cancel()
		if err == nil && id != "" {
			r := turnResult(t, id)
			r.QueueID = res.QueueID
			return r, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return res, false
		}
		select {
		case <-ctx.Done():
			return res, false
		case <-time.After(req.Poll):
		}
	}
}

// findTurn ищет среди последних ходов тот, что начат одним из clientIDs, и
// говорит, каким именно.
func findTurn(ctx context.Context, s AppServer, thread string, clientIDs ...string) (Turn, string, error) {
	turns, err := s.RecentTurns(ctx, thread, 10)
	if err != nil {
		return Turn{}, "", err
	}
	for _, t := range turns {
		for _, id := range t.ClientIDs() {
			for _, want := range clientIDs {
				if want != "" && id == want {
					return t, id, nil
				}
			}
		}
	}
	return Turn{}, "", nil
}

func turnResult(t Turn, clientID string) WakeResult {
	r := WakeResult{Stage: StageStarted, Transport: TransportAppServer, TurnID: t.ID,
		TurnStatus: t.Status, ClientID: clientID}
	if t.Status != "" && t.Status != TurnInProgress {
		r.Stage = StageCompleted
	}
	return r
}

func profileUnusable(ps []PermissionProfile, err error) string {
	if err != nil {
		return "список permission profiles не получен: " + err.Error()
	}
	for _, p := range ps {
		if p.ID == WakePermissionProfile {
			if !p.Allowed {
				return "профиль " + WakePermissionProfile + " запрещён требованиями"
			}
			return ""
		}
	}
	return "профиля " + WakePermissionProfile + " нет в конфиге Codex"
}

func held(r WakeResult, hold, why string) WakeResult {
	r.Hold, r.Reason = hold, why
	return r
}

func own(q []QueuedItem, req WakeRequest) (QueuedItem, bool) {
	for _, it := range q {
		if it.ClientID == req.ClientID {
			return it, true
		}
	}
	for _, it := range q {
		if req.Task != "" && strings.HasPrefix(it.Text(), TextPrefix+req.Task) {
			return it, true
		}
	}
	return QueuedItem{}, false
}

func foreign(q []QueuedItem) bool {
	for _, it := range q {
		if !strings.HasPrefix(it.ClientID, ClientIDPrefix) && !strings.HasPrefix(it.Text(), TextPrefix) {
			return true
		}
	}
	return false
}
