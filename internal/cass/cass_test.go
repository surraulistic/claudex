package cass

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Форма снята с живого ответа cass 16.09: hits + _meta ровно теми именами,
// которые объявляет контракт. Выдумывать её нельзя — на выдуманной схеме
// разбор зелёный, а на настоящей молчит.
const liveJSON = `{
  "total_matches": 4,
  "hits": [
    {"source_path":"/p/-10-01a0a51e-20c4-7e10-8488-b524b9389489.jsonl","source_id":"local",
     "line_number":849,"created_at":1789508673587,"score":0.0163,"match_type":"exact",
     "snippet":"доставка **отчёта** в тред","title":"t","workspace":"/w","agent":"claude_code"}
  ],
  "_meta": {
    "search_mode":"hybrid","requested_search_mode":"hybrid","mode_defaulted":true,
    "semantic_refinement":true,"refinement_level":"fully_hybrid_refined",
    "fallback_tier":null,"fallback_reason":null,"semantic_fallback_reason":null,
    "timed_out":false,"timeout_ms":120000,"elapsed_ms":11432,
    "index_freshness":{"exists":true,"status":"ready","fresh":true,"stale":false,
      "last_indexed_at":"2026-09-16T00:46:27.338+00:00","age_seconds":815,
      "rebuilding":false,"partial":false}
  }
}`

func withRun(t *testing.T, f func(context.Context, []string) ([]byte, error)) {
	t.Helper()
	prev := Run
	Run = f
	t.Cleanup(func() { Run = prev })
}

func TestLiveContractIsParsedWithProvenance(t *testing.T) {
	var got []string
	withRun(t, func(_ context.Context, args []string) ([]byte, error) {
		got = args
		return []byte(liveJSON), nil
	})

	r, err := Search(context.Background(), "доставка отчёта", Options{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if r.Meta.SearchMode != "hybrid" || !r.Meta.SemanticRefinement {
		t.Fatalf("режим и доводка разобраны, получено %+v", r.Meta)
	}
	if r.Meta.RefinementLevel != "fully_hybrid_refined" {
		t.Fatalf("уровень доводки, получено %q", r.Meta.RefinementLevel)
	}
	if d := r.Meta.Degraded(ModeHybrid); d != "" {
		t.Fatalf("ухудшения нет, получено %q", d)
	}
	if n := r.Meta.StaleNote(); n != "" {
		t.Fatalf("индекс свеж, получено %q", n)
	}
	if len(r.Hits) != 1 {
		t.Fatalf("одно попадание, получено %d", len(r.Hits))
	}
	if r.Hits[0].Session != "01a0a51e-20c4-7e10-8488-b524b9389489" {
		t.Fatalf("разговор вытащен из пути, получено %q", r.Hits[0].Session)
	}
	// Официальный интерфейс, а не база: в доводах только cass search.
	joined := strings.Join(got, " ")
	for _, want := range []string{"search", "--json", "--robot-meta", "--mode hybrid"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("в вызове нет %q: %v", want, got)
		}
	}
}

func TestCyrillicQueryGoesThroughUnchanged(t *testing.T) {
	// Запрос уходит доводом процесса, а не через шелл: экранировать нечего, и
	// кириллица не должна ни обрезаться, ни перекодироваться.
	const q = "почему отчёт не дошёл до ведущего"
	var got []string
	withRun(t, func(_ context.Context, args []string) ([]byte, error) {
		got = args
		return []byte(liveJSON), nil
	})
	if _, err := Search(context.Background(), q, Options{}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range got {
		if a == q {
			found = true
		}
	}
	if !found {
		t.Fatalf("запрос ушёл как есть, получено %v", got)
	}
}

func TestSilentSemanticFallbackIsSurfaced(t *testing.T) {
	// Ради этого интеграция и читает _meta: cass отдаёт удачный ответ, молча
	// свалившись на лексику, и без разбора это неотличимо от семантики.
	withRun(t, func(context.Context, []string) ([]byte, error) {
		return []byte(`{"hits":[],"_meta":{"search_mode":"lexical",
			"requested_search_mode":"hybrid","semantic_refinement":false,
			"semantic_fallback_reason":"model not installed",
			"index_freshness":{"fresh":true}}}`), nil
	})
	r, err := Search(context.Background(), "запрос", Options{Mode: ModeHybrid})
	if err != nil {
		t.Fatal(err)
	}
	d := r.Meta.Degraded(ModeHybrid)
	if d == "" || !strings.Contains(d, "model not installed") {
		t.Fatalf("причина отката названа, получено %q", d)
	}
}

func TestTimeoutIsBoundedAndNamed(t *testing.T) {
	withRun(t, func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	start := time.Now()
	_, err := Search(context.Background(), "запрос", Options{Timeout: 40 * time.Millisecond})
	if err == nil {
		t.Fatal("предел соблюдён — это ошибка, а не пустая выдача")
	}
	if !strings.Contains(err.Error(), "не ответил") {
		t.Fatalf("сказано, что именно случилось, получено %q", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("ждали дольше предела: %s", time.Since(start))
	}
}

func TestCassUnavailableIsAnErrorNotAnEmptyResult(t *testing.T) {
	// Пустая выдача и «cass не установлен» — разные вещи. Свалить их в одно
	// значит тихо ухудшить поиск.
	withRun(t, func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("executable file not found")
	})
	if _, err := Search(context.Background(), "запрос", Options{}); err == nil {
		t.Fatal("недоступность — ошибка")
	}
}

func TestBrokenJSONIsNotMistakenForNoResults(t *testing.T) {
	withRun(t, func(context.Context, []string) ([]byte, error) {
		return []byte("это не json"), nil
	})
	if _, err := Search(context.Background(), "запрос", Options{}); err == nil {
		t.Fatal("неразобранный ответ — ошибка")
	}
}

func TestStaleAndRebuildingIndexAreNamed(t *testing.T) {
	m := Meta{Fresh: false, Stale: true, AgeSeconds: 7200, LastIndexedAt: "вчера"}
	if !strings.Contains(m.StaleNote(), "7200") {
		t.Fatalf("отставание названо, получено %q", m.StaleNote())
	}
	if n := (Meta{Rebuilding: true}).StaleNote(); !strings.Contains(n, "перестраивается") {
		t.Fatalf("перестройка названа, получено %q", n)
	}
	if n := (Meta{Fresh: true}).StaleNote(); n != "" {
		t.Fatalf("свежий индекс молчит, получено %q", n)
	}
}

func TestMergeMarksWhatBothEnginesFound(t *testing.T) {
	sem := []Hit{
		{Session: "s1", TS: 1000_000, Snippet: "**совпало**", Score: 0.9, MatchType: "semantic"},
		{Session: "s2", TS: 2000_000, Snippet: "только семантика", Score: 0.8},
	}
	lex := []Local{
		{ID: 7, Session: "s1", TS: 1000, Text: "исходная запись"},
		{ID: 9, Session: "s3", TS: 3000, Text: "только лексика"},
	}
	got := Merge(sem, lex, 0)

	if len(got) != 3 {
		t.Fatalf("три разных попадания, получено %d: %+v", len(got), got)
	}
	if got[0].Via != ViaBoth || got[0].ID != 7 {
		t.Fatalf("найденное обоими помечено и несёт локальный id, получено %+v", got[0])
	}
	if got[0].Text != "исходная запись" {
		t.Fatalf("текст берётся свой, а не фрагмент с подсветкой, получено %q", got[0].Text)
	}
	if got[1].Via != ViaCass {
		t.Fatalf("семантическое второе, получено %+v", got[1])
	}
	if got[2].Via != ViaFTS || got[2].ID != 9 {
		t.Fatalf("лексическое не выброшено, получено %+v", got[2])
	}
}

func TestSemanticRankingIsKeptAheadOfLexicalOnly(t *testing.T) {
	// Порядок cass — это ранжирование, а не случайность; переставлять его
	// своим счётом нельзя. Лексические идут следом, свежие первыми.
	sem := []Hit{{Session: "a", TS: 1000, Snippet: "первое", Score: 0.1}}
	lex := []Local{
		{ID: 1, Session: "b", TS: 5, Text: "старое"},
		{ID: 2, Session: "c", TS: 500, Text: "свежее"},
	}
	got := Merge(sem, lex, 0)
	if got[0].Session != "a" || got[1].Session != "c" || got[2].Session != "b" {
		t.Fatalf("порядок: семантика, затем свежая лексика; получено %+v", got)
	}
}

func TestMergeRespectsTheLimit(t *testing.T) {
	var lex []Local
	for i := 0; i < 50; i++ {
		lex = append(lex, Local{ID: int64(i), Session: "s", TS: int64(i)})
	}
	if n := len(Merge(nil, lex, 5)); n != 5 {
		t.Fatalf("предел соблюдён, получено %d", n)
	}
}

func TestDuplicateSemanticHitsCollapse(t *testing.T) {
	// Один разговор отдаёт несколько строк одной секунды — в выдачу идёт одна.
	sem := []Hit{
		{Session: "s", TS: 1000_000, Snippet: "раз", Line: 10},
		{Session: "s", TS: 1000_400, Snippet: "два", Line: 11},
	}
	if n := len(Merge(sem, nil, 0)); n != 1 {
		t.Fatalf("дубль схлопнут, получено %d", n)
	}
}

func TestSessionOfHandlesCodexRolloutNames(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{"/p/01a0a51e-20c4-7e10-8488-b524b9389489.jsonl", "01a0a51e-20c4-7e10-8488-b524b9389489"},
		{"/p/rollout-2026-09-16T10-10-01a0a51e-20c4-7e10-8488-b524b9389489.jsonl",
			"01a0a51e-20c4-7e10-8488-b524b9389489"},
		{"/p/короткое.jsonl", "короткое"},
	} {
		if got := SessionOf(c.path); got != c.want {
			t.Errorf("SessionOf(%q) = %q, ожидалось %q", c.path, got, c.want)
		}
	}
}

func TestLexicalOnlyHitsKeepSlotsUnderTheLimit(t *testing.T) {
	// На живом архиве при пределе 40 cass отдал все сорок, и лексических в
	// выдаче не осталось вовсе: объединение выродилось в один движок.
	var sem []Hit
	for i := 0; i < 40; i++ {
		sem = append(sem, Hit{Session: "c", TS: int64(i) * 1000, Snippet: "семантика"})
	}
	var lex []Local
	for i := 0; i < 10; i++ {
		lex = append(lex, Local{ID: int64(i), Session: "l", TS: int64(1000 + i), Text: "лексика"})
	}
	got := Merge(sem, lex, 40)

	if len(got) != 40 {
		t.Fatalf("предел соблюдён, получено %d", len(got))
	}
	var fts int
	for _, m := range got {
		if m.Via == ViaFTS {
			fts++
		}
	}
	if fts == 0 {
		t.Fatal("лексическим оставлены места")
	}
	if got[0].Via != ViaCass {
		t.Fatalf("семантика по-прежнему впереди, получено %+v", got[0])
	}
}
