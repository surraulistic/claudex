package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/herdr/herdrtest"
)

func agentList(panes ...[2]string) string {
	var items []string
	for _, p := range panes {
		items = append(items, fmt.Sprintf(
			`{"pane_id":%q,"agent":"claude","agent_status":%q,"terminal_title_stripped":"т","revision":5}`,
			p[0], p[1]))
	}
	out := `{"id":"x","result":{"type":"agent_list","agents":[`
	for i, it := range items {
		if i > 0 {
			out += ","
		}
		out += it
	}
	return out + `]}}`
}

func statusEvent(pane, status string) string {
	return fmt.Sprintf(`{"event":"pane_agent_status_changed","data":{"pane_id":%q,"agent_status":%q,"workspace_id":"wE"}}`, pane, status)
}

type recorder struct {
	mu   sync.Mutex
	seen []Change
}

func (r *recorder) add(c Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, c)
}

func (r *recorder) wait(t *testing.T, n int) []Change {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := len(r.seen)
		r.mu.Unlock()
		if got >= n {
			r.mu.Lock()
			defer r.mu.Unlock()
			return append([]Change(nil), r.seen...)
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("ждал %d изменений, пришло %d: %+v", n, len(r.seen), r.seen)
	return nil
}

func start(t *testing.T, f *herdrtest.Fake, opts Options) (*Watcher, *recorder) {
	t.Helper()
	rec := &recorder{}
	if opts.Reconcile == 0 {
		opts.Reconcile = time.Hour
	}
	if opts.Retry == 0 {
		opts.Retry = 10 * time.Millisecond
	}
	opts.OnChange = rec.add
	w := New(herdr.New(f.Path), opts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	select {
	case <-w.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("наблюдатель не поднялся")
	}
	return w, rec
}

func TestBaselineComesFromAgentList(t *testing.T) {
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}, [2]string{"wE:p2", "working"}))
	w, _ := start(t, f, Options{})
	if n := len(w.State().Panes()); n != 2 {
		t.Fatalf("срез со старта, панелей %d", n)
	}
}

func TestSubscribesToStatusPerPaneAndLifecycleGlobally(t *testing.T) {
	// Постатусная подписка требует pane_id, жизненный цикл — наоборот
	// глобален. И всё это уходит одним запросом: второй в то же соединение
	// сервер не примет.
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}, [2]string{"wE:p2", "idle"}))
	start(t, f, Options{})

	req := f.LastRequest("events.subscribe")
	subs := req["params"].(map[string]any)["subscriptions"].([]any)
	kinds := map[string]int{}
	for _, s := range subs {
		m := s.(map[string]any)
		kinds[m["type"].(string)]++
		if m["type"] == "pane.agent_status_changed" && m["pane_id"] == nil {
			t.Fatal("подписка на статус без pane_id herdr не примет")
		}
		if m["type"] == "pane.updated" {
			t.Fatal("pane.updated не нужен: шум без единого достоверного поля")
		}
	}
	if kinds["pane.agent_status_changed"] != 2 {
		t.Fatalf("по подписке на каждую панель, получено %d", kinds["pane.agent_status_changed"])
	}
	for _, k := range []string{"pane.created", "pane.closed", "pane.exited", "pane.agent_detected"} {
		if kinds[k] != 1 {
			t.Fatalf("жизненный цикл %s не выписан", k)
		}
	}
}

func TestStatusTransitionIsReported(t *testing.T) {
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	w, rec := start(t, f, Options{})
	f.Push(statusEvent("wE:p1", "working"))

	got := rec.wait(t, 1)[0]
	if got.PaneID != "wE:p1" || got.From != "idle" || got.To != "working" {
		t.Fatalf("переход с обеих сторон, получено %+v", got)
	}
	if p, _ := w.State().Get("wE:p1"); p.Status != "working" {
		t.Fatalf("срез подвинулся, получено %q", p.Status)
	}
}

func TestNewPaneTriggersResubscribe(t *testing.T) {
	// Подписаться на статус новой панели по живому соединению нельзя: herdr
	// берёт один запрос на соединение. Значит переподписка.
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	start(t, f, Options{})
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}, [2]string{"wE:p9", "unknown"}))
	f.Push(`{"event":"pane_created","data":{"type":"pane_created","pane":{"pane_id":"wE:p9","agent_status":"unknown","revision":0}}}`)

	f.WaitSubscriptions(t, 2)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req := f.LastRequest("events.subscribe")
		subs := req["params"].(map[string]any)["subscriptions"].([]any)
		for _, s := range subs {
			m := s.(map[string]any)
			if m["type"] == "pane.agent_status_changed" && m["pane_id"] == "wE:p9" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("новая панель не попала в подписку")
}

func TestSubscriptionDropIsSurvived(t *testing.T) {
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	_, rec := start(t, f, Options{})
	f.DropSubscriptions()
	f.WaitSubscriptions(t, 2)

	f.Push(statusEvent("wE:p1", "done"))
	if got := rec.wait(t, 1)[0]; got.To != "done" {
		t.Fatalf("после обрыва события снова доходят, получено %+v", got)
	}
}

func TestReconcileRepairsMissedTransition(t *testing.T) {
	// Событие может не дойти — тогда расхождение находит сверка, а не тишина.
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	_, rec := start(t, f, Options{Reconcile: 20 * time.Millisecond})
	f.Reply("agent.list", agentList([2]string{"wE:p1", "done"}))

	got := rec.wait(t, 1)[0]
	if got.PaneID != "wE:p1" || got.To != "done" || !got.ByReconcile {
		t.Fatalf("сверка чинит и сообщает об этом, получено %+v", got)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	w := New(herdr.New(f.Path), Options{Reconcile: time.Hour, Retry: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	<-w.Ready()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("останов возвращает причину")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run не вернулся после отмены")
	}
}

func TestUnreachableHerdrIsRetried(t *testing.T) {
	f := herdrtest.Start(t)
	f.Reply("agent.list", `{"id":"x","error":{"code":"boom","message":"herdr прилёг"}}`)
	w := New(herdr.New(f.Path), Options{Reconcile: time.Hour, Retry: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(60 * time.Millisecond)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	select {
	case <-w.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("после возвращения herdr наблюдатель обязан подняться")
	}
	var n int
	for _, r := range f.Requests() {
		var m map[string]any
		json.Unmarshal([]byte(r), &m)
		if m["method"] == "agent.list" {
			n++
		}
	}
	if n < 3 {
		t.Fatalf("попытки повторялись, запросов agent.list: %d", n)
	}
}

func TestStatusChangeDuringTheSubscribeWindowIsNotLost(t *testing.T) {
	// С herdr 0.9.0 подписка начинается с живых событий и накопленного не
	// повторяет (#1270). Всё, что случилось между снимком и подпиской, иначе
	// теряется навсегда — а это ровно панель, закончившая работу в этот миг.
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "working"}))
	f.Delay("events.subscribe", 300*time.Millisecond)

	go func() {
		time.Sleep(60 * time.Millisecond)
		f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	}()

	// Сверка отключена часовым интервалом: увидеть смену можно только вторым
	// снимком, и больше ничем.
	_, rec := start(t, f, Options{Reconcile: time.Hour})
	got := rec.wait(t, 1)

	if got[0].PaneID != "wE:p1" || got[0].From != "working" || got[0].To != "idle" {
		t.Fatalf("смена в окне подписки замечена, получено %+v", got[0])
	}
	if !got[0].ByReconcile {
		t.Fatal("найдено снимком, а не событием — так и должно быть помечено")
	}
}

func TestPaneBornInTheSubscribeWindowGetsItsSubscription(t *testing.T) {
	// Панель, появившаяся в окне, осталась бы без подписки на свой статус:
	// событие о её рождении тоже потеряно.
	f := herdrtest.Start(t)
	f.Reply("agent.list", agentList([2]string{"wE:p1", "idle"}))
	f.Delay("events.subscribe", 300*time.Millisecond)

	go func() {
		time.Sleep(60 * time.Millisecond)
		f.Reply("agent.list",
			agentList([2]string{"wE:p1", "idle"}, [2]string{"wE:p2", "working"}))
	}()

	start(t, f, Options{Reconcile: time.Hour})

	req := f.LastRequest("events.subscribe")
	if !strings.Contains(fmt.Sprint(req), "wE:p2") {
		t.Fatalf("переподписались с новой панелью, получено %v", req)
	}
}
