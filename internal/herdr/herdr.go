// Package herdr — единственное место в программе, которое знает про сокет
// herdr. Кадрирование здесь неочевидно: сервер принимает ОДИН запрос на
// соединение и закрывает его после ответа. Исключение — events.subscribe:
// одна подписка может нести список панелей, и тогда соединение остаётся жить,
// а сервер шлёт в него события.
package herdr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const defaultTimeout = 15 * time.Second

// CheapLines — сколько строк можно попросить у панели даром. Запрос сверх
// видимой части экрана заставляет herdr восстанавливать прокрутку: измерено,
// 44 строки на десять панелей стоят 3 мс, 48 строк — 1092 мс. Высота панели в
// этой установке 44, потолок взят с запасом.
const CheapLines = 40

func DefaultSocket() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	if s := os.Getenv("HERDR_SESSION"); s != "" {
		return filepath.Join(home, ".config", "herdr", "sessions", s, "herdr.sock")
	}
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

type Client struct {
	Path    string
	Timeout time.Duration
}

func New(path string) *Client { return &Client{Path: path, Timeout: defaultTimeout} }

type Agent struct {
	PaneID   string `json:"pane_id"`
	TabID    string `json:"tab_id"`
	Name     string `json:"name"`
	Kind     string `json:"agent"`
	Status   string `json:"agent_status"`
	Title    string `json:"terminal_title_stripped"`
	CWD      string `json:"cwd"`
	Revision int64  `json:"revision"`
	Focused  bool   `json:"focused"`
	Session  struct {
		Value string `json:"value"`
	} `json:"agent_session"`
}

type Tab struct {
	TabID string `json:"tab_id"`
	Label string `json:"label"`
}

// Error — отказ самого herdr. Код нужен отдельно от текста: «не дождались» и
// «сломалось во время ожидания» — разные исходы для вызывающего, и различить
// их по подстроке в сообщении нельзя.
type Error struct {
	Method  string
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("herdr %s: %s (%s)", e.Method, e.Message, e.Code)
}

// CodeOf достаёт код отказа сквозь обёртки; пустая строка значит, что отказал
// не herdr.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) dial() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", c.Path, c.Timeout)
	if err != nil {
		return nil, fmt.Errorf("herdr недоступен по %s: %w", c.Path, err)
	}
	return conn, nil
}

func request(conn net.Conn, method string, params any) error {
	if params == nil {
		params = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"id": "claudex", "method": method, "params": params})
	if err != nil {
		return err
	}
	_, err = conn.Write(append(body, '\n'))
	return err
}

// Call — один запрос на своём соединении. Переиспользовать соединение нельзя:
// сервер закрывает его после ответа.
func (c *Client) Call(method string, params any, out any) error {
	return c.call(method, params, out, c.Timeout)
}

// callFor — вызов с собственным сроком. Срок передаётся отдельно, а не правкой
// поля клиента: клиент общий, им одновременно пользуется наблюдатель.
func (c *Client) callFor(method string, params any, out any, extra time.Duration) error {
	return c.call(method, params, out, c.Timeout+extra)
}

func (c *Client) call(method string, params any, out any, timeout time.Duration) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if err := request(conn, method, params); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("herdr закрыл соединение без ответа на %s: %w", method, err)
	}
	var env envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return fmt.Errorf("нечитаемый ответ herdr на %s: %w", method, err)
	}
	if env.Error != nil {
		return &Error{Method: method, Code: env.Error.Code, Message: env.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func (c *Client) Agents() ([]Agent, error) {
	var out struct {
		Agents []Agent `json:"agents"`
	}
	err := c.Call("agent.list", nil, &out)
	return out.Agents, err
}

func (c *Client) Tabs() ([]Tab, error) {
	var out struct {
		Tabs []Tab `json:"tabs"`
	}
	err := c.Call("tab.list", nil, &out)
	return out.Tabs, err
}

func (c *Client) Get(target string) (Agent, error) {
	var out struct {
		Agent Agent `json:"agent"`
	}
	err := c.Call("agent.get", map[string]any{"target": target}, &out)
	return out.Agent, err
}

// Read отдаёт текст экрана. Источник выбирает вызывающий: на работающей панели
// recent отдаёт пусто, на покоящейся — вчетверо больше строк, чем visible.
func (c *Client) Read(target, source string, lines int) (string, error) {
	var out struct {
		Read struct {
			Text string `json:"text"`
		} `json:"read"`
	}
	err := c.Call("agent.read", map[string]any{
		"target": target, "source": source, "lines": lines, "format": "text",
	}, &out)
	return out.Read.Text, err
}

// Подписка. Имя вида в запросе — через точку (pane.agent_status_changed),
// а в пришедшем событии то же самое приезжает через подчёркивание
// (pane_agent_status_changed). Match заполняется только для
// pane.output_matched: тогда herdr сам сторожит вывод панели по образцу и
// присылает совпавшую строку — это точный признак завершения, в отличие от
// опроса статуса.
type Sub struct {
	Kind   string
	PaneID string
	Match  string // подстрока; herdr ждёт её в конверте {type,value}
	Regex  bool
	Source string
	Lines  int
}

func (s Sub) params() map[string]any {
	p := map[string]any{"type": s.Kind}
	if s.PaneID != "" {
		p["pane_id"] = s.PaneID
	}
	if s.Match != "" {
		kind := "substring"
		if s.Regex {
			kind = "regex"
		}
		p["match"] = map[string]any{"type": kind, "value": s.Match}
		p["source"] = orElse(s.Source, "visible")
		p["strip_ansi"] = true
		if s.Lines > 0 {
			p["lines"] = s.Lines
		}
	}
	return p
}

func orElse(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func StatusSubs(panes []string) []Sub {
	subs := make([]Sub, 0, len(panes))
	for _, p := range panes {
		subs = append(subs, Sub{Kind: "pane.agent_status_changed", PaneID: p})
	}
	return subs
}

type Event struct {
	Kind        string          // как на проводе: pane_agent_status_changed
	PaneID      string          `json:"pane_id"`
	Status      string          `json:"agent_status"`
	Agent       string          `json:"agent"`
	Title       string          `json:"title"`
	MatchedLine string          `json:"matched_line"`
	Pane        *Agent          `json:"pane"`
	Read        *readResult     `json:"read"`
	Data        json.RawMessage `json:"-"`
}

type readResult struct {
	Text string `json:"text"`
}

type Subscription struct {
	conn   net.Conn
	events chan Event
	done   chan struct{}
	err    error
}

func (s *Subscription) Events() <-chan Event  { return s.events }
func (s *Subscription) Done() <-chan struct{} { return s.done }
func (s *Subscription) Err() error            { return s.err }
func (s *Subscription) Close() error          { return s.conn.Close() }

// Subscribe подписывается ОДНИМ запросом: второй запрос в то же соединение
// сервер не примет, а подписки на pane.created и прочий жизненный цикл идут
// без pane_id и потому глобальны.
func (c *Client) Subscribe(specs []Sub) (*Subscription, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		list = append(list, s.params())
	}
	if err := request(conn, "events.subscribe", map[string]any{"subscriptions": list}); err != nil {
		conn.Close()
		return nil, err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("подписка не подтверждена: %w", err)
	}
	var env envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil || env.Error != nil {
		conn.Close()
		return nil, fmt.Errorf("подписка отклонена: %s", line)
	}

	s := &Subscription{conn: conn, events: make(chan Event, 64), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.events)
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				// Обрыв — это «не знаем»; путать его с «панель не дошла» нельзя.
				s.err = fmt.Errorf("подписка оборвалась: %w", err)
				return
			}
			if ev, ok := parseEvent([]byte(l)); ok {
				s.events <- ev
			}
		}
	}()
	return s, nil
}

func parseEvent(line []byte) (Event, bool) {
	var wire struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if json.Unmarshal(line, &wire) != nil || wire.Event == "" {
		return Event{}, false
	}
	var ev Event
	json.Unmarshal(wire.Data, &ev)
	ev.Kind = wire.Event
	ev.Data = wire.Data
	if ev.PaneID == "" && ev.Pane != nil {
		ev.PaneID = ev.Pane.PaneID
	}
	return ev, true
}

func (e Event) Text() string {
	if e.Read == nil {
		return ""
	}
	return e.Read.Text
}

// Prompt отправляет задание и, если until не пуст, тем же вызовом ждёт, пока
// панель не придёт в одно из перечисленных состояний. Одним вызовом — чтобы
// между отправкой и началом ожидания не было щели, в которую проваливается
// быстрый ответ.
func (c *Client) Prompt(target, text string, until []string, timeout time.Duration) (Agent, error) {
	params := map[string]any{"target": target, "text": text}
	if len(until) > 0 {
		wait := map[string]any{"until": until}
		if timeout > 0 {
			wait["timeout_ms"] = timeout.Milliseconds()
		}
		params["wait"] = wait
	}
	var out struct {
		Agent Agent `json:"agent"`
	}
	err := c.callFor("agent.prompt", params, &out, timeout)
	return out.Agent, err
}

func (c *Client) Wait(target string, until []string, timeout time.Duration) (Agent, error) {
	params := map[string]any{"target": target, "until": until}
	if timeout > 0 {
		params["timeout_ms"] = timeout.Milliseconds()
	}
	var out struct {
		Agent Agent `json:"agent"`
	}
	err := c.callFor("agent.wait", params, &out, timeout)
	return out.Agent, err
}
