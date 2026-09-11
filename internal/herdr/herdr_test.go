package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Поддельный herdr — настоящий unix-сокет, а не мок. Именно на границе
// кадрирования легко ошибиться: сервер берёт ОДИН запрос на соединение, но
// один запрос может нести много подписок, и тогда соединение остаётся жить.
type fake struct {
	path    string
	replies map[string]string // метод → сырой ответ
	events  chan string       // строки, которые сервер шлёт в открытую подписку
	seen    chan string       // методы, которые сервер получил
	body    string            // последний запрос целиком
}

func newFake(t *testing.T) *fake {
	t.Helper()
	// Путь юникс-сокета обрезается на 104 байтах: t.TempDir() подставляет имя
	// теста и на длинных именах bind падает с invalid argument.
	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fake{
		path:    filepath.Join(dir, "h.sock"),
		replies: map[string]string{},
		events:  make(chan string, 8),
		seen:    make(chan string, 32),
	}
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		t.Fatalf("сокет: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fake) serve(c net.Conn) {
	defer c.Close()
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return
	}
	var req struct {
		Method string `json:"method"`
	}
	json.Unmarshal([]byte(line), &req)
	f.body = line
	select {
	case f.seen <- req.Method:
	default:
	}
	reply, ok := f.replies[req.Method]
	if !ok {
		reply = `{"id":"x","error":{"code":"invalid_request","message":"unknown variant"}}`
	}
	c.Write([]byte(reply + "\n"))
	if req.Method == "events.subscribe" {
		// подписка держит соединение и шлёт события, пока их дают
		for ev := range f.events {
			if _, err := c.Write([]byte(ev + "\n")); err != nil {
				break
			}
		}
		return
	}
	// один запрос на соединение: defer закроет его сразу после ответа
}

func TestCallReturnsResult(t *testing.T) {
	f := newFake(t)
	f.replies["agent.list"] = `{"id":"x","result":{"type":"agent_list","agents":[{"pane_id":"wE:p1","agent":"claude","agent_status":"idle"}]}}`
	c := New(f.path)
	var got struct {
		Agents []Agent `json:"agents"`
	}
	if err := c.Call("agent.list", nil, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 || got.Agents[0].PaneID != "wE:p1" {
		t.Fatalf("разбор ответа, получено %+v", got.Agents)
	}
}

func TestEachCallUsesItsOwnConnection(t *testing.T) {
	// Сервер закрывает соединение после ответа; клиент обязан открывать новое,
	// иначе второй вызов молча провалится.
	f := newFake(t)
	f.replies["agent.list"] = `{"id":"x","result":{"agents":[]}}`
	c := New(f.path)
	for i := 0; i < 3; i++ {
		var out struct{}
		if err := c.Call("agent.list", nil, &out); err != nil {
			t.Fatalf("вызов %d: %v", i+1, err)
		}
	}
}

func TestErrorEnvelopeBecomesError(t *testing.T) {
	f := newFake(t)
	f.replies["agent.get"] = `{"id":"x","error":{"code":"agent_not_found","message":"нет такой"}}`
	var out struct{}
	err := New(f.path).Call("agent.get", map[string]any{"target": "нет"}, &out)
	if err == nil || !strings.Contains(err.Error(), "agent_not_found") {
		t.Fatalf("конверт ошибки становится ошибкой, получено %v", err)
	}
}

func TestUnreachableSocketIsError(t *testing.T) {
	var out struct{}
	if err := New("/нет/такого.sock").Call("ping", nil, &out); err == nil {
		t.Fatal("недоступный сокет — ошибка, а не тишина")
	}
}

func TestReadReturnsScreenText(t *testing.T) {
	f := newFake(t)
	f.replies["agent.read"] = `{"id":"x","result":{"type":"read","read":{"pane_id":"wE:p1","source":"visible","text":"⏺ готово\n"}}}`
	text, err := New(f.path).Read("wE:p1", "visible", 40)
	if err != nil || !strings.Contains(text, "готово") {
		t.Fatalf("чтение отдаёт текст экрана, получено %q %v", text, err)
	}
}

// Строки ниже сняты с живого сокета herdr, а не придуманы: имя вида в подписке
// пишется через точку, а в событии приезжает через подчёркивание, и полезная
// часть лежит в data, а не в корне. На обоих я в этой сессии ошибся.
const (
	evStatus   = `{"event":"pane_agent_status_changed","data":{"agent":"claude","agent_status":"idle","pane_id":"wE:p2","workspace_id":"wE","title":"готово"}}`
	evCreated  = `{"event":"pane_created","data":{"type":"pane_created","pane":{"pane_id":"wE:p1S","agent_status":"unknown","cwd":"/tmp"},"workspace_id":"wE"}}`
	evClosed   = `{"event":"pane_closed","data":{"pane_id":"wE:p1K","type":"pane_closed","workspace_id":"wE"}}`
	evDetected = `{"event":"pane_agent_detected","data":{"agent":"claude","pane_id":"wE:p1S","type":"pane_agent_detected","workspace_id":"wE"}}`
	evMatched  = `{"event":"pane_output_matched","data":{"pane_id":"wE:p2","matched_line":"ГОТОВО-7","read":{"text":"строка\nГОТОВО-7\n","source":"visible"}}}`
)

func subscribed(t *testing.T, specs []Sub) (*fake, *Subscription) {
	t.Helper()
	f := newFake(t)
	f.replies["events.subscribe"] = `{"id":"s","result":{"type":"subscription_started"}}`
	sub, err := New(f.path).Subscribe(specs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sub.Close() })
	<-f.seen
	return f, sub
}

func next(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatalf("поток событий закрыт: %v", sub.Err())
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("событие не пришло — соединение не держится")
	}
	return Event{}
}

func TestSubscribeSendsOneRequestForEveryPane(t *testing.T) {
	// Десять отдельных запросов в одно соединение сервер закрывает: все панели
	// уходят одним запросом.
	f, _ := subscribed(t, StatusSubs([]string{"wE:p1", "wE:p2", "wE:p3"}))
	var req struct {
		Params struct {
			Subscriptions []map[string]any `json:"subscriptions"`
		} `json:"params"`
	}
	json.Unmarshal([]byte(f.body), &req)
	if n := len(req.Params.Subscriptions); n != 3 {
		t.Fatalf("три панели одним запросом, подписок в запросе: %d", n)
	}
	if k := req.Params.Subscriptions[0]["type"]; k != "pane.agent_status_changed" {
		t.Fatalf("в запросе вид пишется через точку, получено %v", k)
	}
}

func TestStatusEventIsParsedFromDataNotRoot(t *testing.T) {
	f, sub := subscribed(t, StatusSubs([]string{"wE:p2"}))
	f.events <- evStatus
	ev := next(t, sub)
	if ev.Kind != "pane_agent_status_changed" {
		t.Fatalf("вид события как на проводе, получено %q", ev.Kind)
	}
	if ev.PaneID != "wE:p2" || ev.Status != "idle" || ev.Agent != "claude" {
		t.Fatalf("полезная часть лежит в data, получено %+v", ev)
	}
}

func TestLifecycleEventsCarryPaneID(t *testing.T) {
	// У pane_created идентификатор спрятан внутрь вложенного pane, у остальных
	// лежит в корне data. Без выравнивания половина событий приходит безымянной.
	f, sub := subscribed(t, []Sub{{Kind: "pane.created"}, {Kind: "pane.closed"}, {Kind: "pane.agent_detected"}})
	for _, want := range []struct{ line, kind, pane string }{
		{evCreated, "pane_created", "wE:p1S"},
		{evClosed, "pane_closed", "wE:p1K"},
		{evDetected, "pane_agent_detected", "wE:p1S"},
	} {
		f.events <- want.line
		ev := next(t, sub)
		if ev.Kind != want.kind || ev.PaneID != want.pane {
			t.Fatalf("%s: получено вид=%q панель=%q", want.kind, ev.Kind, ev.PaneID)
		}
	}
}

func TestOutputMatchSubscriptionCarriesPattern(t *testing.T) {
	// herdr сам сторожит вывод по образцу и присылает совпавшую строку —
	// это точный признак завершения, а не догадка по статусу.
	f, sub := subscribed(t, []Sub{{Kind: "pane.output_matched", PaneID: "wE:p2", Match: "ГОТОВО-7"}})
	var req struct {
		Params struct {
			Subscriptions []map[string]any `json:"subscriptions"`
		} `json:"params"`
	}
	json.Unmarshal([]byte(f.body), &req)
	if got := req.Params.Subscriptions[0]["match"]; got != "ГОТОВО-7" {
		t.Fatalf("образец уходит на сервер, получено %v", got)
	}
	f.events <- evMatched
	ev := next(t, sub)
	if ev.MatchedLine != "ГОТОВО-7" || !strings.Contains(ev.Text(), "ГОТОВО-7") {
		t.Fatalf("совпавшая строка и экран, получено %+v", ev)
	}
}

func TestSubscriptionDropIsReported(t *testing.T) {
	// Обрыв — это «мы не знаем», и его нельзя спутать с «панель не дошла».
	f, sub := subscribed(t, StatusSubs([]string{"wE:p1"}))
	close(f.events)
	select {
	case <-sub.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("обрыв подписки должен быть сигналом, а не тишиной")
	}
	if sub.Err() == nil {
		t.Fatal("причина обрыва сохраняется")
	}
}
