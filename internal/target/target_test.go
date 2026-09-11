package target

import (
	"testing"

	"github.com/surraulistic/claudex/internal/state"
)

func panes() []state.Pane {
	return []state.Pane{
		{ID: "wE:p1", Kind: "claude", Title: "MEDIA-UNIFICATION", CWD: "/п/медиа",
			Name: "media", SessionID: "7e7dd560-896f-4d96-9618-1924a89087a3"},
		{ID: "wE:p13", Kind: "claude", Title: "license", CWD: "/п/казино"},
		{ID: "wE:p17", Kind: "codex", Title: "gjvj-2", CWD: "/п/кодекс"},
	}
}

var labels = map[string]string{"wE:p13": "install", "wE:p17": "codex install"}

func TestPaneIDWins(t *testing.T) {
	p, err := Resolve("wE:p13", panes(), labels)
	if err != nil || p.ID != "wE:p13" {
		t.Fatalf("панель по её идентификатору, получено %+v %v", p, err)
	}
}

func TestSessionIDIsAccepted(t *testing.T) {
	// Прошлый сбой: find выдавал session_id, а адресоваться им было нельзя.
	p, err := Resolve("7e7dd560-896f-4d96-9618-1924a89087a3", panes(), labels)
	if err != nil || p.ID != "wE:p1" {
		t.Fatalf("панель по идентификатору сессии, получено %+v %v", p, err)
	}
}

func TestTitleSubstringIsCaseInsensitive(t *testing.T) {
	p, err := Resolve("media", panes(), labels)
	if err != nil || p.ID != "wE:p1" {
		t.Fatalf("панель по куску заголовка, получено %+v %v", p, err)
	}
}

func TestCWDSuffixMatches(t *testing.T) {
	p, err := Resolve("/п/кодекс", panes(), labels)
	if err != nil || p.ID != "wE:p17" {
		t.Fatalf("панель по рабочему каталогу, получено %+v %v", p, err)
	}
}

func TestAmbiguousTargetIsRefusedWithCandidates(t *testing.T) {
	// Наугад выбирать нельзя: задание уедет в чужой разговор.
	list := append(panes(), state.Pane{ID: "wE:p20", Title: "license-2", CWD: "/п/иное"})
	_, err := Resolve("licen", list, labels)
	if err == nil {
		t.Fatal("двусмысленная цель отклоняется")
	}
	for _, want := range []string{"wE:p13", "wE:p20"} {
		if !contains(err.Error(), want) {
			t.Fatalf("в отказе перечислены кандидаты, получено %q", err)
		}
	}
}

func TestExactTitleBeatsSubstring(t *testing.T) {
	list := append(panes(), state.Pane{ID: "wE:p20", Title: "license-cleanup", CWD: "/п/иное"})
	p, err := Resolve("license", list, labels)
	if err != nil || p.ID != "wE:p13" {
		t.Fatalf("точное совпадение заголовка снимает двусмысленность, получено %+v %v", p, err)
	}
}

func TestUnknownTargetNamesWhatWasTried(t *testing.T) {
	_, err := Resolve("нетакой", panes(), labels)
	if err == nil || !contains(err.Error(), "нетакой") {
		t.Fatalf("отказ называет цель, получено %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestAliasAndLabelAreAccepted(t *testing.T) {
	// Навык обещает адресовать панель её меткой, и сам инструмент выдаёт метку
	// в поле target. Раньше «media» совпадало случайно — как подстрока
	// заголовка MEDIA-UNIFICATION, а имя без общих букв с заголовком не
	// находилось вовсе.
	for _, c := range []struct{ q, want string }{
		{"media", "wE:p1"},          // имя агента
		{"install", "wE:p13"},       // метка вкладки
		{"codex install", "wE:p17"}, // метка с пробелом
	} {
		p, err := Resolve(c.q, panes(), labels)
		if err != nil || p.ID != c.want {
			t.Fatalf("%q → %+v %v, ожидалась %s", c.q, p, err, c.want)
		}
	}
}

func TestAliasBeatsSubstringOfAnotherTitle(t *testing.T) {
	// Точное имя сильнее случайного вхождения в чужой заголовок.
	list := append(panes(), state.Pane{ID: "wE:p20", Title: "работа с media-файлами"})
	p, err := Resolve("media", list, labels)
	if err != nil || p.ID != "wE:p1" {
		t.Fatalf("получено %+v %v", p, err)
	}
}

func TestLabelSubstringStillWorks(t *testing.T) {
	p, err := Resolve("codex", panes(), labels)
	if err != nil || p.ID != "wE:p17" {
		t.Fatalf("получено %+v %v", p, err)
	}
}
