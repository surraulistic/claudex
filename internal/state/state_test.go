package state

import (
	"testing"

	"github.com/surraulistic/claudex/internal/herdr"
)

func ev(kind, pane, status, title string, rev int64) herdr.Event {
	e := herdr.Event{Kind: kind, PaneID: pane, Status: status, Title: title}
	if kind == "pane_updated" || kind == "pane_created" {
		e.Pane = &herdr.Agent{PaneID: pane, Status: status, Title: title, Revision: rev}
		e.Status, e.Title = "", ""
	}
	return e
}

func loaded() *State {
	s := New()
	s.Load([]herdr.Agent{
		{PaneID: "wE:p1", Kind: "claude", Status: "idle", Title: "один", Revision: 10},
		{PaneID: "wE:p2", Kind: "codex", Status: "working", Title: "два", Revision: 20},
	})
	return s
}

func TestLoadTakesBaseline(t *testing.T) {
	s := loaded()
	if len(s.Panes()) != 2 {
		t.Fatalf("две панели, получено %d", len(s.Panes()))
	}
	p, ok := s.Get("wE:p2")
	if !ok || p.Kind != "codex" || p.Status != "working" {
		t.Fatalf("панель из среза, получено %+v", p)
	}
}

func TestUpdateAdvancesRevisionWithoutTouchingAgentFields(t *testing.T) {
	s := loaded()
	s.Apply(ev("pane_updated", "wE:p1", "idle", "один", 11))
	if p, _ := s.Get("wE:p1"); p.Revision != 11 {
		t.Fatalf("revision учтён, получено %d", p.Revision)
	}
}

func TestAgentFieldsInUpdatedEventAreIgnored(t *testing.T) {
	// Измерено на живом herdr. Статус внутри pane_updated скачет
	// done→blocked→working несколько раз в секунду, пока agent.get держит одно
	// значение; заголовок панели Codex приезжает анимацией
	// «[ ! ] Action Required | gjvj-2», которую agent.get срезает до «gjvj-2».
	// Поверить любому из них — будить ведущего на шуме.
	s := loaded()
	rev := int64(11)
	for _, noise := range []struct{ status, title string }{
		{"blocked", "[ ! ] Action Required | один"},
		{"working", "[ . ] Action Required | один"},
		{"unknown", "[ ! ] Action Required | один"},
	} {
		rev++
		if s.Apply(ev("pane_updated", "wE:p1", noise.status, noise.title, rev)) {
			t.Fatalf("кадр экрана не изменение, но %q сочтён им", noise.status)
		}
		p, _ := s.Get("wE:p1")
		if p.Status != "idle" || p.Title != "один" {
			t.Fatalf("поля агента не берутся из кадра, получено статус=%q загл=%q", p.Status, p.Title)
		}
	}
}

func TestStatusChangeEventIsAuthoritative(t *testing.T) {
	s := loaded()
	if !s.Apply(herdr.Event{Kind: "pane_agent_status_changed", PaneID: "wE:p1", Status: "working"}) {
		t.Fatal("настоящий переход — значимое изменение")
	}
	if p, _ := s.Get("wE:p1"); p.Status != "working" {
		t.Fatalf("статус взят, получено %q", p.Status)
	}
}

func TestReconcileRepairsDrift(t *testing.T) {
	// Событие может не дойти; сверка возвращает панели, где срез разошёлся
	// с herdr, чтобы демон не унёс расхождение в делегирование.
	s := loaded()
	changed := s.Reconcile([]herdr.Agent{
		{PaneID: "wE:p1", Kind: "claude", Status: "done", Title: "один", Revision: 12},
		{PaneID: "wE:p2", Kind: "codex", Status: "working", Title: "два", Revision: 20},
	})
	if len(changed) != 1 || changed[0] != "wE:p1" {
		t.Fatalf("разошлась одна панель, получено %v", changed)
	}
	if p, _ := s.Get("wE:p1"); p.Status != "done" {
		t.Fatalf("сверка авторитетна, получено %q", p.Status)
	}
	if len(s.Reconcile([]herdr.Agent{
		{PaneID: "wE:p1", Kind: "claude", Status: "done", Title: "один", Revision: 12},
	})) != 1 {
		t.Fatal("исчезнувшая панель тоже расхождение")
	}
	if _, ok := s.Get("wE:p2"); ok {
		t.Fatal("сверка убирает пропавшую панель")
	}
}

func TestReplayedEventIsIgnored(t *testing.T) {
	// herdr проигрывает новому подписчику прошлые события: pane_created панели
	// приезжает с revision 0, когда она давно живёт на revision 3. Измерено на
	// живом сокете дважды с разницей в восемь минут.
	s := loaded()
	if s.Apply(ev("pane_created", "wE:p1", "unknown", "", 0)) {
		t.Fatal("повтор создания не меняет состояние")
	}
	if p, _ := s.Get("wE:p1"); p.Status != "idle" || p.Title != "один" {
		t.Fatalf("живая панель не затёрта повтором, получено %+v", p)
	}
	if s.Apply(ev("pane_updated", "wE:p2", "idle", "два", 19)) {
		t.Fatal("событие старше известного не применяется")
	}
	if p, _ := s.Get("wE:p2"); p.Status != "working" {
		t.Fatalf("статус не откатился, получено %q", p.Status)
	}
}

func TestScrollChurnIsNotMeaningful(t *testing.T) {
	// Одна деятельная панель даёт 324 события за 45 секунд: revision растёт на
	// каждую строку вывода. Пересчитывать что-либо на каждое такое событие
	// нельзя, поэтому значимой считается только смена статуса, заголовка,
	// каталога или вида агента.
	s := loaded()
	for rev := int64(11); rev < 40; rev++ {
		if s.Apply(ev("pane_updated", "wE:p1", "idle", "один", rev)) {
			t.Fatalf("прокрутка не значима, но revision %d сочтён изменением", rev)
		}
	}
	if p, _ := s.Get("wE:p1"); p.Revision != 39 {
		t.Fatalf("revision всё равно учтён, получено %d", p.Revision)
	}
}

func TestCreateAndCloseTrackLifecycle(t *testing.T) {
	s := loaded()
	if !s.Apply(ev("pane_created", "wE:p9", "unknown", "", 0)) {
		t.Fatal("новая панель — значимое изменение")
	}
	if len(s.Panes()) != 3 {
		t.Fatalf("панель добавилась, получено %d", len(s.Panes()))
	}
	if !s.Apply(herdr.Event{Kind: "pane_closed", PaneID: "wE:p9"}) {
		t.Fatal("закрытие — значимое изменение")
	}
	if _, ok := s.Get("wE:p9"); ok {
		t.Fatal("закрытая панель убрана")
	}
	if s.Apply(herdr.Event{Kind: "pane_closed", PaneID: "wE:pНЕТ"}) {
		t.Fatal("закрытие неизвестной панели ничего не меняет")
	}
}

func TestAgentDetectedFillsKind(t *testing.T) {
	// Панель заводится раньше, чем herdr опознаёт агента: у созданной вид пуст.
	s := loaded()
	s.Apply(ev("pane_created", "wE:p9", "unknown", "", 0))
	if !s.Apply(herdr.Event{Kind: "pane_agent_detected", PaneID: "wE:p9", Agent: "claude"}) {
		t.Fatal("опознание агента — значимое изменение")
	}
	if p, _ := s.Get("wE:p9"); p.Kind != "claude" {
		t.Fatalf("вид агента проставлен, получено %q", p.Kind)
	}
}

func TestPanesAreSortedAndCopied(t *testing.T) {
	s := loaded()
	list := s.Panes()
	list[0].Status = "испорчено"
	if p, _ := s.Get(list[0].ID); p.Status == "испорчено" {
		t.Fatal("выдача — копия, править её снаружи нельзя")
	}
	if s.Panes()[0].ID > s.Panes()[1].ID {
		t.Fatal("порядок устойчив")
	}
}

func TestUnknownEventIsHarmless(t *testing.T) {
	s := loaded()
	if s.Apply(herdr.Event{Kind: "layout_updated"}) {
		t.Fatal("незнакомое событие ничего не меняет")
	}
	if len(s.Panes()) != 2 {
		t.Fatal("и никого не теряет")
	}
}
