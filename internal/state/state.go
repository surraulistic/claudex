// Package state держит срез панелей в памяти и правит его событиями herdr.
//
// Два свойства потока событий определяют весь разбор. Первое: herdr проигрывает
// новому подписчику прошлые события, поэтому применение идёт по revision и
// повтор просто отбрасывается. Второе: деятельная панель шлёт событие на
// каждую строку вывода, поэтому значимым считается не всякое изменение, а
// только смена статуса, заголовка, каталога или вида агента.
package state

import (
	"sort"
	"sync"

	"github.com/surraulistic/claudex/internal/herdr"
)

type Pane struct {
	ID        string
	TabID     string
	Kind      string
	Status    string
	Title     string
	CWD       string
	SessionID string
	Revision  int64
	Focused   bool
}

func fromAgent(a herdr.Agent) Pane {
	return Pane{
		ID: a.PaneID, TabID: a.TabID, Kind: a.Kind, Status: a.Status,
		Title: a.Title, CWD: a.CWD, SessionID: a.Session.Value,
		Revision: a.Revision, Focused: a.Focused,
	}
}

// значимо ли изменение для того, кто смотрит на панели снаружи
func (p Pane) differs(o Pane) bool {
	return p.Status != o.Status || p.Title != o.Title || p.CWD != o.CWD ||
		p.Kind != o.Kind || p.TabID != o.TabID
}

// Reconcile приводит срез к тому, что herdr считает правдой, и возвращает
// панели, по которым срез разошёлся.
func (s *State) Reconcile(agents []herdr.Agent) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var changed []string
	seen := make(map[string]bool, len(agents))
	for _, a := range agents {
		seen[a.PaneID] = true
		next := fromAgent(a)
		prev, known := s.panes[a.PaneID]
		if known && !next.differs(prev) {
			s.panes[a.PaneID] = next
			continue
		}
		s.panes[a.PaneID] = next
		changed = append(changed, a.PaneID)
	}
	for id := range s.panes {
		if !seen[id] {
			delete(s.panes, id)
			changed = append(changed, id)
		}
	}
	sort.Strings(changed)
	return changed
}

type State struct {
	mu    sync.RWMutex
	panes map[string]Pane
}

func New() *State { return &State{panes: map[string]Pane{}} }

func (s *State) Load(agents []herdr.Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panes = make(map[string]Pane, len(agents))
	for _, a := range agents {
		s.panes[a.PaneID] = fromAgent(a)
	}
}

// Apply возвращает true, только если снаружи стало видно что-то другое.
func (s *State) Apply(ev herdr.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch ev.Kind {
	case "pane_created", "pane_updated":
		if ev.Pane == nil {
			return false
		}
		next := fromAgent(*ev.Pane)
		prev, known := s.panes[next.ID]
		if known && next.Revision < prev.Revision {
			return false // повтор из буфера herdr
		}
		if known {
			// В событии не всякое поле заполнено; пустое не затирает известное.
			next.Kind = orKeep(next.Kind, prev.Kind)
			next.SessionID = orKeep(next.SessionID, prev.SessionID)
			next.TabID = orKeep(next.TabID, prev.TabID)
			// В pane_updated можно верить только структурным полям. Поля,
			// относящиеся к агенту, там — сырой кадр экрана: статус скачет
			// done→blocked→working по нескольку раз в секунду, а заголовок
			// панели Codex приезжает анимацией «[ ! ] Action Required»,
			// которую agent.get срезает. Измерено на живом herdr.
			next.Status = prev.Status
			next.Title = prev.Title
		}
		s.panes[next.ID] = next
		return !known || next.differs(prev)

	case "pane_closed", "pane_exited":
		if _, ok := s.panes[ev.PaneID]; !ok {
			return false
		}
		delete(s.panes, ev.PaneID)
		return true

	case "pane_agent_detected":
		p, ok := s.panes[ev.PaneID]
		if !ok || p.Kind == ev.Agent {
			return false
		}
		p.Kind = ev.Agent
		s.panes[ev.PaneID] = p
		return true

	case "pane_agent_status_changed":
		p, ok := s.panes[ev.PaneID]
		if !ok || p.Status == ev.Status {
			return false
		}
		p.Status = ev.Status
		if ev.Title != "" {
			p.Title = ev.Title
		}
		s.panes[ev.PaneID] = p
		return true
	}
	return false
}

func orKeep(next, prev string) string {
	if next == "" {
		return prev
	}
	return next
}

func (s *State) Get(id string) (Pane, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.panes[id]
	return p, ok
}

func (s *State) Panes() []Pane {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Pane, 0, len(s.panes))
	for _, p := range s.panes {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
