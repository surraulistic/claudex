// Package watch держит срез панелей свежим: одно живое соединение с herdr,
// одна подписка на все панели и сверка как страховка.
//
// Подписка на статус требует pane_id, поэтому набор подписок привязан к списку
// панелей, а появление новой заставляет переподписаться — по живому соединению
// добавить подписку нельзя, herdr берёт один запрос на соединение.
package watch

import (
	"context"
	"errors"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/state"
)

type Change struct {
	PaneID      string
	From        string
	To          string
	Pane        state.Pane
	ByReconcile bool // нашла сверка, а не событие: значит событие не дошло
}

type Options struct {
	Reconcile time.Duration
	Retry     time.Duration
	OnChange  func(Change)
}

type Watcher struct {
	client *herdr.Client
	state  *state.State
	opts   Options
	ready  chan struct{}
	loaded bool
}

func New(c *herdr.Client, o Options) *Watcher {
	if o.Reconcile <= 0 {
		o.Reconcile = time.Minute
	}
	if o.Retry <= 0 {
		o.Retry = 2 * time.Second
	}
	return &Watcher{client: c, state: state.New(), opts: o, ready: make(chan struct{})}
}

func (w *Watcher) State() *state.State    { return w.state }
func (w *Watcher) Ready() <-chan struct{} { return w.ready }

// errResubscribe — не сбой, а требование пересобрать подписку, поэтому пауза
// перед новой попыткой не берётся.
var errResubscribe = errors.New("нужна переподписка")

func (w *Watcher) Run(ctx context.Context) error {
	for {
		err := w.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errResubscribe) {
			continue
		}
		select {
		case <-time.After(w.opts.Retry):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (w *Watcher) session(ctx context.Context) error {
	agents, err := w.client.Agents()
	if err != nil {
		return err
	}
	w.absorb(agents)

	panes := w.state.Panes()
	specs := []herdr.Sub{
		{Kind: "pane.created"}, {Kind: "pane.closed"},
		{Kind: "pane.exited"}, {Kind: "pane.agent_detected"},
	}
	for _, p := range panes {
		specs = append(specs, herdr.Sub{Kind: "pane.agent_status_changed", PaneID: p.ID})
	}
	sub, err := w.client.Subscribe(specs)
	if err != nil {
		return err
	}
	defer sub.Close()
	w.markReady()

	tick := time.NewTicker(w.opts.Reconcile)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-sub.Events():
			if !ok {
				return sub.Err()
			}
			w.handle(ev)
			if ev.Kind == "pane_created" {
				return errResubscribe
			}
		case <-tick.C:
			if agents, err := w.client.Agents(); err == nil {
				w.absorb(agents)
			}
		}
	}
}

func (w *Watcher) handle(ev herdr.Event) {
	before, known := w.state.Get(ev.PaneID)
	if !w.state.Apply(ev) {
		return
	}
	after, alive := w.state.Get(ev.PaneID)
	if !alive || !known || before.Status == after.Status {
		return
	}
	w.notify(Change{PaneID: ev.PaneID, From: before.Status, To: after.Status, Pane: after})
}

// absorb принимает авторитетный список: первый раз как срез, дальше как сверку,
// и сообщает о статусах, про которые событие не дошло.
func (w *Watcher) absorb(agents []herdr.Agent) {
	if !w.loaded {
		w.state.Load(agents)
		w.loaded = true
		return
	}
	before := map[string]string{}
	for _, p := range w.state.Panes() {
		before[p.ID] = p.Status
	}
	for _, id := range w.state.Reconcile(agents) {
		p, alive := w.state.Get(id)
		if !alive || before[id] == p.Status {
			continue
		}
		w.notify(Change{PaneID: id, From: before[id], To: p.Status, Pane: p, ByReconcile: true})
	}
}

func (w *Watcher) notify(c Change) {
	if w.opts.OnChange != nil {
		w.opts.OnChange(c)
	}
}

func (w *Watcher) markReady() {
	select {
	case <-w.ready:
	default:
		close(w.ready)
	}
}
