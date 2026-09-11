package herdr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	mu      sync.Mutex
	body    string // последний запрос целиком
}

func (f *fake) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body
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
	f.mu.Lock()
	f.body = line
	f.mu.Unlock()
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
	// Код доступен отдельно от текста: «не дождались» и «сломалось во время
	// ожидания» — разные исходы, и различать их по подстроке нельзя.
	if got := CodeOf(fmt.Errorf("обёртка: %w", err)); got != "agent_not_found" {
		t.Fatalf("код сквозь обёртку, получено %q", got)
	}
	if CodeOf(errors.New("не от herdr")) != "" {
		t.Fatal("чужая ошибка кода не даёт")
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
	json.Unmarshal([]byte(f.lastBody()), &req)
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
	json.Unmarshal([]byte(f.lastBody()), &req)
	// herdr ждёт образец конвертом {type,value}, а не голой строкой: голую он
	// отвергает как invalid_request.
	m, ok := req.Params.Subscriptions[0]["match"].(map[string]any)
	if !ok || m["type"] != "substring" || m["value"] != "ГОТОВО-7" {
		t.Fatalf("образец конвертом, получено %#v", req.Params.Subscriptions[0]["match"])
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

func TestPromptCarriesAtomicWait(t *testing.T) {
	// Отправка и ожидание одним вызовом: между ними не должно быть щели, в
	// которую проваливается быстрый ответ.
	f := newFake(t)
	f.replies["agent.prompt"] = `{"id":"x","result":{"type":"agent","agent":{"pane_id":"wE:p2","agent_status":"idle"}}}`
	a, err := New(f.path).Prompt("wE:p2", "сделай", []string{"idle", "done"}, 90*time.Second)
	if err != nil || a.Status != "idle" {
		t.Fatalf("ответ разобран, получено %+v %v", a, err)
	}
	var req struct {
		Params struct {
			Target string `json:"target"`
			Text   string `json:"text"`
			Wait   struct {
				Until     []string `json:"until"`
				TimeoutMS int64    `json:"timeout_ms"`
			} `json:"wait"`
		} `json:"params"`
	}
	json.Unmarshal([]byte(f.lastBody()), &req)
	if req.Params.Target != "wE:p2" || req.Params.Text != "сделай" {
		t.Fatalf("цель и текст, получено %+v", req.Params)
	}
	if len(req.Params.Wait.Until) != 2 || req.Params.Wait.TimeoutMS != 90000 {
		t.Fatalf("ожидание уехало вместе с заданием, получено %+v", req.Params.Wait)
	}
}

func TestPromptWithoutUntilDoesNotWait(t *testing.T) {
	f := newFake(t)
	f.replies["agent.prompt"] = `{"id":"x","result":{"type":"agent","agent":{"pane_id":"wE:p2"}}}`
	if _, err := New(f.path).Prompt("wE:p2", "сделай", nil, 0); err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	json.Unmarshal([]byte(f.lastBody()), &req)
	if _, has := req["params"].(map[string]any)["wait"]; has {
		t.Fatal("без until ожидание не выписывается")
	}
}

func TestLongWaitDoesNotDisturbConcurrentCalls(t *testing.T) {
	// Клиент общий: пока одна горутина ждёт задание минутами, другая обязана
	// ходить со своим обычным сроком. Правка поля клиента дала бы гонку.
	f := newFake(t)
	f.replies["agent.prompt"] = `{"id":"x","result":{"agent":{"pane_id":"wE:p2"}}}`
	f.replies["agent.list"] = `{"id":"x","result":{"agents":[]}}`
	c := New(f.path)
	c.Timeout = 2 * time.Second

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				c.Prompt("wE:p2", "сделай", []string{"idle"}, time.Hour)
			} else {
				c.Agents()
			}
		}(i)
	}
	wg.Wait()
	if c.Timeout != 2*time.Second {
		t.Fatalf("срок клиента не трогается, стал %v", c.Timeout)
	}
}

func TestNotifyShowsToUser(t *testing.T) {
	f := newFake(t)
	f.replies["notification.show"] = `{"id":"x","result":{"type":"notification_show","shown":true}}`
	if err := New(f.path).Notify("готово", "задача 742309b7"); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Params map[string]any `json:"params"`
	}
	json.Unmarshal([]byte(f.lastBody()), &req)
	if req.Params["title"] != "готово" || req.Params["body"] != "задача 742309b7" {
		t.Fatalf("заголовок и текст доезжают, получено %v", req.Params)
	}
}
