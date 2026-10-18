package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Socket — управляющий сокет уже запущенного app-server (`codex remote-control
// start`). Свой сервер claudex не поднимает никогда: тред должен проснуться в
// том же демоне, что и у приложения, иначе у него окажется два писателя.
func Socket() string {
	if p := strings.TrimSpace(os.Getenv("CLAUDEX_CODEX_SOCKET")); p != "" {
		return p
	}
	h := Home()
	if h == "" {
		return ""
	}
	return filepath.Join(h, "app-server-control", "app-server-control.sock")
}

// Состояния треда так, как их называет app-server.
const (
	StatusNotLoaded   = "notLoaded"
	StatusIdle        = "idle"
	StatusActive      = "active"
	StatusSystemError = "systemError"
)

type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

// WaitingOnHuman — ход стоит на решении человека.
func (s ThreadStatus) WaitingOnHuman() bool {
	for _, f := range s.ActiveFlags {
		if f == "waitingOnApproval" || f == "waitingOnUserInput" {
			return true
		}
	}
	return false
}

type Thread struct {
	ID        string       `json:"id"`
	Status    ThreadStatus `json:"status"`
	Ephemeral bool         `json:"ephemeral"`
	Cwd       string       `json:"cwd,omitempty"`
}

type PermissionProfile struct {
	ID      string `json:"id"`
	Allowed bool   `json:"allowed"`
}

// Resumed — что сервер сказал о треде после thread/resume.
type Resumed struct {
	Thread Thread
	// Profile — activePermissionProfile.id; пусто, если сервер его не назвал.
	Profile string
	// Sandbox — тип устаревшего поля sandbox, например dangerFullAccess.
	Sandbox string
}

// Settings — настройки треда из уведомления thread/settings/updated.
type Settings struct {
	// Profile — activePermissionProfile.id; пусто, если сервер его не назвал.
	Profile string
	// Sandbox — sandboxPolicy.type.
	Sandbox string
}

type QueuedItem struct {
	ID       string      `json:"id"`
	ClientID string      `json:"clientUserMessageId"`
	Input    []UserInput `json:"input"`
}

func (q QueuedItem) Text() string {
	var b strings.Builder
	for _, in := range q.Input {
		b.WriteString(in.Text)
	}
	return b.String()
}

type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Сколько подробностей хода отдаёт thread/turns/list.
const (
	ItemsSummary = "summary"
	ItemsFull    = "full"
)

// Статусы хода.
const (
	TurnInProgress  = "inProgress"
	TurnCompleted   = "completed"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
)

type Turn struct {
	ID     string     `json:"id"`
	Status string     `json:"status"`
	Items  []TurnItem `json:"items"`
	// StartedAt — unix-секунды начала хода; 0, если сервер не назвал.
	StartedAt int64 `json:"startedAt,omitempty"`
}

type TurnItem struct {
	Type     string `json:"type"`
	ClientID string `json:"clientId,omitempty"`
	// Content приходит только при itemsView=full.
	Content []UserInput `json:"content,omitempty"`
}

func (it TurnItem) Text() string {
	var b strings.Builder
	for _, in := range it.Content {
		b.WriteString(in.Text)
	}
	return b.String()
}

// AppServer — то, что пробуждению нужно от app-server. Интерфейс ради
// подделки в тестах: настоящий вызов будит живой разговор.
type AppServer interface {
	ReadThread(ctx context.Context, id string) (Thread, error)
	QueueList(ctx context.Context, thread string) ([]QueuedItem, error)
	QueueAdd(ctx context.Context, thread, clientID, text string) (QueuedItem, error)
	PermissionProfiles(ctx context.Context, cwd string) ([]PermissionProfile, error)
	// Resume загружает тред с заданным permission profile. Настройка липкая,
	// поэтому пробуждение намеренно переводит его на профиль claudex-work.
	// Загруженный тред, открытый другим клиентом, переопределения не
	// принимает: ответ называет действующий профиль, а соединение
	// подписывается на тред.
	Resume(ctx context.Context, thread, permissions string) (Resumed, error)
	// SetPermissions переводит загруженный тред на permission profile и ждёт
	// thread/settings/updated именно этого треда. Ответ на update пуст:
	// применённое видно только в уведомлении, а оно приходит лишь подписчикам
	// треда и лишь при фактической смене.
	SetPermissions(ctx context.Context, thread, permissions string) (Settings, error)
	QueueStart(ctx context.Context, thread, queueID string) (Turn, error)
	// TurnsPage — страница ходов, новые первыми; next пуст, когда ходов
	// больше нет.
	TurnsPage(ctx context.Context, thread, cursor string, limit int, view string) (turns []Turn, next string, err error)
	Close() error
}

// RPCError — отказ самого app-server, в отличие от обрыва связи.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("app-server %s: %s (код %d)", e.Method, e.Message, e.Code)
}

// Dial подключается к работающему app-server. Подменяется в тестах.
var Dial = func(ctx context.Context) (AppServer, error) {
	return DialSocket(ctx, Socket())
}

type client struct {
	ws   *wsConn
	next int
	// settings — thread/settings/updated по тредам, пришедшие во время чужих
	// вызовов; последнее перекрывает прежние.
	settings map[string]Settings
}

func DialSocket(ctx context.Context, path string) (AppServer, error) {
	if path == "" {
		return nil, errors.New("путь к сокету app-server неизвестен")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("app-server недоступен: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	ws, err := wsHandshake(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	cl := &client{ws: ws, settings: map[string]Settings{}}
	// experimentalApi нужен ради thread/queue/* и permissions. Из уведомлений
	// нужно одно — thread/settings/updated; исход хода читается опросом, а
	// поток дельт только занимает канал.
	if err := cl.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "claudex", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true,
			"optOutNotificationMethods": noisyNotifications},
	}, nil); err != nil {
		ws.close()
		return nil, err
	}
	if err := cl.notify("initialized"); err != nil {
		ws.close()
		return nil, err
	}
	return cl, nil
}

var noisyNotifications = []string{
	"item/agentMessage/delta", "item/reasoning/textDelta", "item/reasoning/summaryTextDelta",
	"item/commandExecution/outputDelta", "item/fileChange/outputDelta",
	"mcpServer/startupStatus/updated", "thread/tokenUsage/updated",
}

func (c *client) Close() error { return c.ws.close() }

func (c *client) notify(method string) error {
	b, err := json.Marshal(map[string]any{"method": method})
	if err != nil {
		return err
	}
	return c.ws.writeFrame(opText, b)
}

type rpcReply struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call ждёт ответа на свой запрос. Уведомления пропускаются. Встречные
// запросы сервера — одобрения, ввод пользователя — остаются без ответа
// намеренно: решать их claudex не вправе, а молчание оставляет их человеку.
func (c *client) call(ctx context.Context, method string, params, out any) error {
	c.next++
	id := c.next
	b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	c.deadline(ctx)
	if err := c.ws.writeFrame(opText, b); err != nil {
		return fmt.Errorf("app-server %s: %w", method, err)
	}
	for {
		r, err := c.recv()
		if err != nil {
			return fmt.Errorf("app-server %s: %w", method, err)
		}
		if r.ID == nil || r.Method != "" || *r.ID != id {
			continue
		}
		if r.Error != nil {
			return &RPCError{Method: method, Code: r.Error.Code, Message: r.Error.Message}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("app-server %s: разбор ответа: %w", method, err)
		}
		return nil
	}
}

func (c *client) deadline(ctx context.Context) {
	if dl, ok := ctx.Deadline(); ok {
		c.ws.c.SetDeadline(dl)
	} else {
		c.ws.c.SetDeadline(time.Time{})
	}
}

// recv читает одно сообщение и запоминает thread/settings/updated: оно может
// прийти и до ответа на update, и после.
func (c *client) recv() (rpcReply, error) {
	msg, err := c.ws.readMessage()
	if err != nil {
		return rpcReply{}, err
	}
	var r rpcReply
	if json.Unmarshal(msg, &r) != nil {
		return rpcReply{}, nil
	}
	if r.ID == nil && r.Method == "thread/settings/updated" {
		var n struct {
			ThreadID string `json:"threadId"`
			Settings struct {
				Active *struct {
					ID string `json:"id"`
				} `json:"activePermissionProfile"`
				Sandbox struct {
					Type string `json:"type"`
				} `json:"sandboxPolicy"`
			} `json:"threadSettings"`
		}
		if json.Unmarshal(r.Params, &n) == nil && n.ThreadID != "" {
			s := Settings{Sandbox: n.Settings.Sandbox.Type}
			if n.Settings.Active != nil {
				s.Profile = n.Settings.Active.ID
			}
			c.settings[n.ThreadID] = s
		}
	}
	return r, nil
}

func (c *client) ReadThread(ctx context.Context, id string) (Thread, error) {
	var r struct {
		Thread Thread `json:"thread"`
	}
	err := c.call(ctx, "thread/read", map[string]any{"threadId": id}, &r)
	return r.Thread, err
}

func (c *client) QueueList(ctx context.Context, thread string) ([]QueuedItem, error) {
	var all []QueuedItem
	var cursor *string
	for page := 0; page < 20; page++ {
		var r struct {
			Data       []QueuedItem `json:"data"`
			NextCursor *string      `json:"nextCursor"`
		}
		p := map[string]any{"threadId": thread}
		if cursor != nil {
			p["cursor"] = *cursor
		}
		if err := c.call(ctx, "thread/queue/list", p, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Data...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			return all, nil
		}
		cursor = r.NextCursor
	}
	return all, errors.New("app-server thread/queue/list: слишком много страниц")
}

func (c *client) QueueAdd(ctx context.Context, thread, clientID, text string) (QueuedItem, error) {
	var r struct {
		Item QueuedItem `json:"queuedSubmission"`
	}
	err := c.call(ctx, "thread/queue/add", map[string]any{
		"threadId": thread, "clientUserMessageId": clientID,
		"input": []UserInput{{Type: "text", Text: text}},
	}, &r)
	return r.Item, err
}

func (c *client) PermissionProfiles(ctx context.Context, cwd string) ([]PermissionProfile, error) {
	var all []PermissionProfile
	var cursor *string
	for page := 0; page < 20; page++ {
		var r struct {
			Data       []PermissionProfile `json:"data"`
			NextCursor *string             `json:"nextCursor"`
		}
		p := map[string]any{}
		if cwd != "" {
			p["cwd"] = cwd
		}
		if cursor != nil {
			p["cursor"] = *cursor
		}
		if err := c.call(ctx, "permissionProfile/list", p, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Data...)
		if r.NextCursor == nil || *r.NextCursor == "" {
			return all, nil
		}
		cursor = r.NextCursor
	}
	return all, errors.New("app-server permissionProfile/list: слишком много страниц")
}

// Resume шлёт permissions без sandbox: вместе их сервер не принимает. Поле
// экспериментальное — без experimentalApi в initialize его нет в протоколе.
func (c *client) Resume(ctx context.Context, thread, permissions string) (Resumed, error) {
	if permissions == "" {
		return Resumed{}, errors.New("thread/resume без permission profile унаследовал бы прежний")
	}
	var r struct {
		Thread Thread `json:"thread"`
		Active *struct {
			ID string `json:"id"`
		} `json:"activePermissionProfile"`
		Sandbox struct {
			Type string `json:"type"`
		} `json:"sandbox"`
	}
	err := c.call(ctx, "thread/resume", map[string]any{
		"threadId": thread, "excludeTurns": true, "permissions": permissions,
	}, &r)
	out := Resumed{Thread: r.Thread, Sandbox: r.Sandbox.Type}
	if r.Active != nil {
		out.Profile = r.Active.ID
	}
	return out, err
}

func (c *client) SetPermissions(ctx context.Context, thread, permissions string) (Settings, error) {
	if permissions == "" {
		return Settings{}, errors.New("thread/settings/update без permission profile ничего не подтвердит")
	}
	// Уведомления до своего запроса — о чужих изменениях.
	delete(c.settings, thread)
	if err := c.call(ctx, "thread/settings/update",
		map[string]any{"threadId": thread, "permissions": permissions}, nil); err != nil {
		return Settings{}, err
	}
	c.deadline(ctx)
	for {
		if s, ok := c.settings[thread]; ok {
			delete(c.settings, thread)
			return s, nil
		}
		if _, err := c.recv(); err != nil {
			return Settings{}, fmt.Errorf("app-server: thread/settings/updated для треда не пришло: %w", err)
		}
	}
}

func (c *client) QueueStart(ctx context.Context, thread, queueID string) (Turn, error) {
	var r struct {
		Turn Turn `json:"turn"`
	}
	err := c.call(ctx, "thread/queue/start",
		map[string]any{"threadId": thread, "queuedSubmissionId": queueID}, &r)
	return r.Turn, err
}

func (c *client) TurnsPage(ctx context.Context, thread, cursor string, limit int, view string) ([]Turn, string, error) {
	var r struct {
		Data       []Turn  `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	p := map[string]any{"threadId": thread, "limit": limit, "itemsView": view}
	if cursor != "" {
		p["cursor"] = cursor
	}
	err := c.call(ctx, "thread/turns/list", p, &r)
	next := ""
	if r.NextCursor != nil {
		next = *r.NextCursor
	}
	return r.Data, next, err
}
