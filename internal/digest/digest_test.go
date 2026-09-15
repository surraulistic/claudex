package digest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/store"
)

var (
	start = time.Date(2026, 9, 15, 21, 0, 0, 0, time.Local)
	stop  = start.Add(20 * time.Minute)
)

type asked struct {
	key    string
	from   int64
	to     int64
	limit  int
	called bool
}

// probe собирает дайджест на поддельных источниках и заодно запоминает, о чём
// именно спросили хранилище: границы окна — половина смысла пакета, и
// проверять их по выходным данным недостаточно.
func probe(t *testing.T, head Head, headErr error, rows []store.Entry, o Options) (Digest, *asked) {
	t.Helper()
	a := &asked{}
	o.Task = "т1"
	o.Key = "разговор"
	o.From, o.To = start, stop
	o.Now = stop.Add(time.Minute)
	o.LookupHead = func(string) (Head, error) { return head, headErr }
	o.Entries = func(k string, from, to int64, limit, chars int) ([]store.Entry, error) {
		a.called, a.key, a.from, a.to, a.limit = true, k, from, to, limit
		return rows, nil
	}
	return Build(o), a
}

func entry(at time.Time, kind, text string, tool bool) store.Entry {
	return store.Entry{TS: at.Unix(), Kind: kind, Text: text, Tool: tool, Len: len(text)}
}

func TestStoreIsAskedExactlyForTheTaskWindow(t *testing.T) {
	// Окно закрыто с обеих сторон. В том же разговоре до поручения шла другая
	// работа, и выдать её за свою — худший вид неверного отчёта.
	_, a := probe(t, Head{LastActivity: stop}, nil, nil, Options{})
	if !a.called {
		t.Fatal("хранилище должны были спросить")
	}
	if a.from != start.UnixMilli() || a.to != stop.UnixMilli() {
		t.Fatalf("границы окна: получено [%d,%d], ожидалось [%d,%d]",
			a.from, a.to, start.UnixMilli(), stop.UnixMilli())
	}
}

func TestRunningTaskClosesTheWindowAtNow(t *testing.T) {
	// Поручение ещё идёт: конца нет, но окно всё равно закрыто — иначе запрос
	// уедет в будущее и захватит чужую работу, начавшуюся следом.
	now := stop.Add(5 * time.Minute)
	a := &asked{}
	Build(Options{
		Task: "т1", Key: "разговор", From: start, Now: now,
		LookupHead: func(string) (Head, error) { return Head{LastActivity: now}, nil },
		Entries: func(k string, from, to int64, limit, chars int) ([]store.Entry, error) {
			a.from, a.to = from, to
			return nil, nil
		},
	})
	if a.to != now.UnixMilli() {
		t.Fatalf("окно закрывается текущим временем, получено %d, ожидалось %d", a.to, now.UnixMilli())
	}
}

func TestIndexBehindTheTaskShowsNothingInsteadOfOlderWork(t *testing.T) {
	// Самое важное правило. Индекс cass отстаёт на часы; если его последняя
	// запись старше начала поручения, про эту работу он не знает ничего.
	// Показать вместо неё более старые записи — соврать уверенно.
	rows := []store.Entry{entry(start.Add(time.Minute), "assistant", "это не наша работа", false)}
	d, a := probe(t, Head{LastActivity: start.Add(-2 * time.Hour)}, nil, rows, Options{})

	if a.called {
		t.Fatal("за записями идти незачем: индекс окно не покрывает")
	}
	if d.Covered {
		t.Fatal("покрытия нет, и это должно быть видно")
	}
	if len(d.Talk) != 0 || len(d.Work) != 0 {
		t.Fatalf("разделы хода пусты, получено talk=%d work=%d", len(d.Talk), len(d.Work))
	}
	if !hasNote(d, "не покрывает окно") {
		t.Fatalf("причина пустоты названа, получено %q", d.Notes)
	}
}

func TestLiveTailSurvivesAnIndexThatKnowsNothing(t *testing.T) {
	// Хвост читается из herdr, а не из индекса, поэтому свежий по построению.
	// Он и остаётся единственным доказательством работы, когда индекс отстал.
	d, _ := probe(t, Head{}, errors.New("разговор не найден"), nil, Options{
		Pane: "wE:p1A",
		Tail: func(string, int) ([]string, error) { return []string{"строка хвоста"}, nil },
	})
	if len(d.Tail) != 1 || d.Tail[0] != "строка хвоста" {
		t.Fatalf("живой хвост на месте, получено %+v", d.Tail)
	}
	if d.Covered {
		t.Fatal("индекса нет — покрытия нет")
	}
	if !hasNote(d, "нет в индексе") {
		t.Fatalf("причина названа, получено %q", d.Notes)
	}
}

func TestMissingSourcesStillProduceAReport(t *testing.T) {
	// Отчёт обязан уйти даже без индекса и без herdr: молчание хуже неполноты.
	d := Build(Options{Task: "т1", From: start, To: stop})
	if d.Covered || len(d.Notes) == 0 {
		t.Fatalf("неполнота названа, получено %+v", d)
	}
	if !strings.Contains(d.Text("готово"), "готово") {
		t.Fatal("сводка остаётся в тексте")
	}
}

func TestLongSessionIsTrimmedAndSaysHowMuch(t *testing.T) {
	// Длинная сессия не должна ни раздуть сообщение, ни молча оборваться:
	// ведущему важно знать, что он видит не всё.
	var rows []store.Entry
	for i := 0; i < 400; i++ {
		rows = append(rows, entry(start.Add(time.Duration(i)*time.Second),
			"assistant", strings.Repeat("я", 500), false))
	}
	d, a := probe(t, Head{LastActivity: stop}, nil, rows, Options{MaxEntries: 20})

	// Предел уходит и в само хранилище. Обрезать уже полученное поздно:
	// запрос за всем разговором — это и есть «огромный транскрипт», от
	// которого пакет защищает.
	if a.limit > 21 {
		t.Fatalf("у хранилища просят ограниченное число строк, получено %d", a.limit)
	}
	if len(d.Talk) > 20 {
		t.Fatalf("записей не больше предела, получено %d", len(d.Talk))
	}
	if d.Dropped == 0 {
		t.Fatal("отброшенное посчитано")
	}
	if !hasNote(d, "не поместилось") {
		t.Fatalf("о неполноте сказано, получено %q", d.Notes)
	}
}

func TestBudgetCapsTheWholeDigestNotJustTheCount(t *testing.T) {
	// Предел по числу записей не спасает: двадцать записей по десять тысяч
	// символов — это всё ещё непомерное сообщение.
	var rows []store.Entry
	for i := 0; i < 20; i++ {
		rows = append(rows, entry(start.Add(time.Duration(i)*time.Second),
			"assistant", strings.Repeat("я", 4000), false))
	}
	d, _ := probe(t, Head{LastActivity: stop}, nil, rows, Options{Budget: 3000, Chars: 4000})

	total := 0
	for _, l := range d.Talk {
		total += len(l.Text)
	}
	if total > 3000 {
		t.Fatalf("бюджет соблюдён, потрачено %d при пределе 3000", total)
	}
	if d.Dropped == 0 {
		t.Fatal("отброшенное посчитано")
	}
}

func TestCommandsAndTheirResultsAreKeptApartFromTalk(t *testing.T) {
	// «Что реально делалось» живёт в записях инструментов. Без них отчёт
	// подменяется пересказом, а с ними вперемешку — не читается.
	rows := []store.Entry{
		entry(start.Add(time.Minute), "assistant", "сейчас соберу", false),
		entry(start.Add(2*time.Minute), "tool", strings.Repeat("в", 900), true),
	}
	d, _ := probe(t, Head{LastActivity: stop}, nil, rows, Options{ToolChars: 100})

	if len(d.Talk) != 1 || len(d.Work) != 1 {
		t.Fatalf("реплики и работа разделены, получено talk=%d work=%d", len(d.Talk), len(d.Work))
	}
	if len([]rune(d.Work[0].Text)) > 120 {
		t.Fatalf("вывод инструмента режется жёстче реплики, получено %d", len([]rune(d.Work[0].Text)))
	}
}

func TestTextAnnouncesItselfAsASummaryAndNamesTheNextStep(t *testing.T) {
	// Требование ради которого всё это: ведущий не должен принять сводку за
	// прочитанный им самим ход работы.
	d, _ := probe(t, Head{LastActivity: stop}, nil,
		[]store.Entry{entry(start.Add(time.Minute), "assistant", "сделано", false)}, Options{})
	text := d.Text("готово: всё собралось")

	for _, want := range []string{"сводка от ClauDex", "claudex digest т1", "git "} {
		if !strings.Contains(text, want) {
			t.Fatalf("в тексте нет %q:\n%s", want, text)
		}
	}
}

func TestStaleIndexIsReportedEvenWhenItCoversTheWindow(t *testing.T) {
	// Покрытие и свежесть — разные вещи. Индекс может дотянуться до начала
	// окна и всё же отставать от файла на часы.
	d, _ := probe(t, Head{LastActivity: start.Add(time.Minute)}, nil,
		[]store.Entry{entry(start.Add(time.Minute), "assistant", "сделано", false)},
		Options{})
	if !d.Covered {
		t.Fatal("окно покрыто")
	}
	if d.Stale == nil {
		t.Fatal("отставание от журнала доказано и должно быть названо")
	}
	if !strings.Contains(d.Text("готово"), d.Stale.Reason) {
		t.Fatal("причина отставания попадает в текст")
	}
}

func hasNote(d Digest, sub string) bool {
	for _, n := range d.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
