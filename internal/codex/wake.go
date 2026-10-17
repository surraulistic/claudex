package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// StageTurnFailed — ход с нашим сообщением кончился failed или
	// interrupted: сообщение в истории треда, но обработано ли — неизвестно.
	// Повтор с новым сообщением мог бы повторить уже сделанное, поэтому
	// дальше решает человек.
	StageTurnFailed = "turn_failed"
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
	// claudex уже переключал права этого треда, а сейчас они другие: их
	// вернули. Повторно менять права живой сессии не наше решение.
	HoldProfileReverted = "permission_profile_reverted"
	HoldTurnFailed      = "turn_failed"
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
	// Since — не позже первой попытки положить сообщение: ход с ним не может
	// начаться раньше, и история треда глубже не листается.
	Since time.Time
	// QueuedByCLI — что журнал клал в этот тред через `codex queue`. Только
	// эти записи без своего clientUserMessageId не считаются чужими.
	QueuedByCLI []CLIQueued
	// QueueOnlyText — что класть через `codex queue`, когда app-server
	// недоступен: ход тогда начнётся с правами того, кто откроет тред.
	QueueOnlyText string
	// KeepProfile — права этого треда claudex уже менял: другой профиль
	// сейчас значит, что их вернули, и повторно не переключаем.
	KeepProfile bool
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
	// ProfileSet — как claudex перевёл тред на claudex-work: resume
	// выгруженного или thread/settings/update загруженного; пусто — не менял.
	ProfileSet string
	// ProfileFrom — профиль до thread/settings/update; пусто — без профиля.
	ProfileFrom string
	// TextHash — TextHash положенного `codex queue`: по нему запись потом
	// узнаётся точно.
	TextHash string
}

// CLIQueued — запись, положенная `codex queue` по поручению Task. Hash пуст
// у записей прежних сборок: тогда остаётся сверка по началу текста.
type CLIQueued struct {
	Task string
	Hash string
}

// TextHash — отпечаток текста записи очереди. Пробелы по краям не в счёт:
// как `codex queue` хранит текст, вживую не сверялось.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:])
}

const (
	ProfileByResume = "resume"
	ProfileByUpdate = "settings_update"
	// ProfileByUpdateUnconfirmed — update послан, но применение не
	// подтверждено: права могли смениться, а могли и нет.
	ProfileByUpdateUnconfirmed = "settings_update_unconfirmed"
	// ProfileByResumeLoaded — resume загруженного треда назвал claudex-work.
	// Ничейный тред сервер перезагружает с переопределением, так что права
	// могли смениться; прежние он не называет.
	ProfileByResumeLoaded = "resume_loaded"
)

// Started — дошло ли до хода.
func (r WakeResult) Started() bool {
	return r.Stage == StageStarted || r.Stage == StageCompleted || r.Stage == StageTurnFailed
}

// Processed — ход с нашим сообщением завершился штатно.
func (r WakeResult) Processed() bool { return r.Stage == StageCompleted }

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

// Follow смотрит, чем кончился ход, ничего не кладя и не запуская. since —
// не позже начала хода.
func Follow(ctx context.Context, thread, clientID string, since time.Time, each time.Duration) (WakeResult, error) {
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
	t, id, err := findTurn(c, s, thread, since, clientID)
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
	text := req.Text
	if req.QueueOnlyText != "" {
		text = req.QueueOnlyText
	}
	c, cancel := context.WithTimeout(ctx, req.Each)
	defer cancel()
	if err := Queue(c, req.Thread, text); err != nil {
		return WakeResult{Transport: TransportQueueOnly},
			fmt.Errorf("app-server недоступен (%v), и codex queue не принял: %w", dialErr, err)
	}
	r.Reason = "принято codex queue; app-server недоступен, ход не запускался: " + dialErr.Error()
	r.TextHash = TextHash(text)
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
	t, id, err := findTurn(c, s, req.Thread, req.Since, req.ClientID, req.SeenClientID)
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
	if foreign(queue, req) {
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
	text := req.Text
	if loaded {
		r, ok := confirmLoaded(ctx, s, req, res)
		if !ok {
			return r, nil
		}
		res = r
		if res.ProfileSet == ProfileByUpdate {
			text += fmt.Sprintf("\n\n(claudex перевёл права этого треда на %s; было: %s)",
				WakePermissionProfile, orNone(res.ProfileFrom))
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
		res.ProfileSet = ProfileByResume
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
		mine, err = s.QueueAdd(c, req.Thread, req.ClientID, text)
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
	r := turnResult(turn, res.ClientID)
	r.QueueID, r.ProfileSet, r.ProfileFrom = res.QueueID, res.ProfileSet, res.ProfileFrom
	return r, nil
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
		res.ProfileSet = ProfileByResumeLoaded
		return res, true
	}
	if req.KeepProfile {
		return held(res, HoldProfileReverted, fmt.Sprintf(
			"claudex уже переводил этот тред на %s, а сейчас у него %s: права вернули, "+
				"и менять их открытой сессии повторно не будем — разбудить тред может человек",
			WakePermissionProfile, orNone(rs.Profile))), false
	}
	c, cancel = context.WithTimeout(ctx, req.Each)
	st, err := s.SetPermissions(c, req.Thread, WakePermissionProfile)
	cancel()
	// Явный отказ сервера — права не менялись; обрыв или тишина — могли.
	var refused *RPCError
	if !errors.As(err, &refused) {
		res.ProfileSet, res.ProfileFrom = ProfileByUpdateUnconfirmed, rs.Profile
	}
	if err != nil {
		return held(res, HoldLoadedProfile,
			"профиль загруженного треда не подтверждён ("+err.Error()+"): ничего не кладём и не запускаем"), false
	}
	if st.Profile != WakePermissionProfile || !safeSandbox(st.Sandbox) {
		return held(res, HoldWrongProfile, fmt.Sprintf(
			"после thread/settings/update профиль %q, sandbox %q вместо %s: ничего не кладём и не запускаем",
			st.Profile, st.Sandbox, WakePermissionProfile)), false
	}
	res.ProfileSet = ProfileByUpdate
	return res, true
}

// safeSandbox — только песочницы самого Codex: externalSandbox, как и
// dangerFullAccess, даёт полный доступ к диску.
func safeSandbox(t string) bool { return t == "readOnly" || t == "workspaceWrite" }

func orNone(profile string) string {
	if profile == "" {
		return "без именованного профиля"
	}
	return profile
}

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
		t, id, err := findTurn(c, s, req.Thread, req.Since, req.ClientID, res.ClientID)
		cancel()
		if err == nil && id != "" {
			r := turnResult(t, id)
			r.QueueID, r.ProfileSet, r.ProfileFrom = res.QueueID, res.ProfileSet, res.ProfileFrom
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

const (
	turnPage     = 50
	turnMaxPages = 40
	// turnSlack — запас на расхождение часов claudex и демона.
	turnSlack = 5 * time.Minute
)

// findTurn ищет ход, начатый одним из clientIDs, листая историю от новых к
// старым до since. Не долистали — ошибка, а не «хода нет»: иначе принятое
// сообщение выглядело бы пропавшим.
func findTurn(ctx context.Context, s AppServer, thread string, since time.Time, clientIDs ...string) (Turn, string, error) {
	floor := int64(0)
	if !since.IsZero() {
		floor = since.Add(-turnSlack).Unix()
	}
	cursor := ""
	for page := 0; page < turnMaxPages; page++ {
		turns, next, err := s.TurnsPage(ctx, thread, cursor, turnPage)
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
		if next == "" || len(turns) == 0 {
			return Turn{}, "", nil
		}
		if last := turns[len(turns)-1].StartedAt; floor > 0 && last > 0 && last < floor {
			return Turn{}, "", nil
		}
		cursor = next
	}
	return Turn{}, "", fmt.Errorf("история треда %s длиннее %d ходов: свой ход не найден и не исключён",
		thread, turnPage*turnMaxPages)
}

func turnResult(t Turn, clientID string) WakeResult {
	r := WakeResult{Stage: StageStarted, Transport: TransportAppServer, TurnID: t.ID,
		TurnStatus: t.Status, ClientID: clientID}
	switch t.Status {
	case "", TurnInProgress:
	case TurnCompleted:
		r.Stage = StageCompleted
	default:
		r.Stage, r.Hold = StageTurnFailed, HoldTurnFailed
		r.Reason = fmt.Sprintf("ход %s кончился статусом %q: событие могло остаться необработанным; "+
			"повторно не кладём, чтобы не повторить сделанное", t.ID, t.Status)
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
		if req.Task != "" && !strings.HasPrefix(it.ClientID, ClientIDPrefix) &&
			strings.HasPrefix(it.Text(), TextPrefix+req.Task) && queuedByCLI(it, req) {
			return it, true
		}
	}
	return QueuedItem{}, false
}

// foreign — есть ли в очереди не наше. Запись без своего clientId признаётся
// нашей, только если журнал сам клал её `codex queue` по этому поручению:
// одного начала текста мало, так может начать сообщение и человек.
func foreign(q []QueuedItem, req WakeRequest) bool {
	for _, it := range q {
		if strings.HasPrefix(it.ClientID, ClientIDPrefix) {
			continue
		}
		if !queuedByCLI(it, req) {
			return true
		}
	}
	return false
}

func queuedByCLI(it QueuedItem, req WakeRequest) bool {
	text := it.Text()
	hash := TextHash(text)
	for _, q := range req.QueuedByCLI {
		switch {
		case q.Hash != "":
			if q.Hash == hash {
				return true
			}
		case q.Task != "" && strings.HasPrefix(text, TextPrefix+q.Task):
			return true
		}
	}
	return false
}
