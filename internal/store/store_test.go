package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const (
	indexed     = "11111111-2222-3333-4444-555555555555"
	cliIngested = "66666666-7777-8888-9999-aaaaaaaaaaaa"
)

// Настоящая sqlite, а не мок: обе ветки резолва, фильтр заглушек и FTS живут
// в SQL, и подделанный слой их не проверяет.
const fixture = `
create table conv (id integer primary key, agent text, session_id text,
  source_path text, workspace text, title text, started_at integer, ended_at integer);
create index conv_session on conv(session_id);
create table msg (id integer primary key, conv_id integer, idx integer,
  role text, created_at integer, len integer, is_tool integer not null default 0);
create index msg_conv on msg(conv_id, is_tool, created_at desc, id desc);
create virtual table msg_fts using fts5(content, tokenize='unicode61 remove_diacritics 2');

insert into conv values
  (1,'claude_code','` + indexed + `','/p/` + indexed + `.jsonl','/g','Работа',1000,2000),
  (2,'codex','` + cliIngested + `','/c/rollout-2026-09-05T10-00-00-` + cliIngested + `.jsonl','/g/wt','Codex',1000,2000);

insert into msg (id,conv_id,idx,role,created_at,len,is_tool) values
  (1,1,0,'user',1788164300000,20,0),
  (2,1,1,'assistant',1788164338000,16,0),
  (3,1,2,'assistant',1788164400000,18,1),
  (4,2,0,'user',1788164500000,18,0);
insert into msg_fts (rowid,content) values
  (1,'подними River на dev'),
  (2,'влит bonuses!874'),
  (3,'[Tool: ToolSearch]'),
  (4,'кэшбек config-service');
`

func open(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("открытие: %v", err)
	}
	if _, err := s.db.Exec(fixture); err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestResolvesByProviderSessionID(t *testing.T) {
	d, err := open(t).Digest(indexed, 8, 400)
	if err != nil {
		t.Fatal(err)
	}
	if d.ConvID != 1 || d.Agent != "claude_code" {
		t.Fatalf("точный ключ, получено conv=%d agent=%q", d.ConvID, d.Agent)
	}
}

func TestResolvesCodexByRolloutFilenameTail(t *testing.T) {
	// У Codex имя файла rollout-<метка>-<uuid>.jsonl: ключом служит хвост.
	d, err := open(t).Digest(cliIngested, 8, 400)
	if err != nil {
		t.Fatal(err)
	}
	if d.Agent != "codex" {
		t.Fatalf("сессия Codex резолвится по хвосту имени, получено %q", d.Agent)
	}
}

func TestResolvesByConversationID(t *testing.T) {
	// find отдаёт id разговора, когда provider_session_id пуст.
	if d, err := open(t).Digest("2", 8, 400); err != nil || d.ConvID != 2 {
		t.Fatalf("цель принимается как id разговора, получено %v %v", d, err)
	}
}

func TestToolStubsExcludedFromTimelineAndCount(t *testing.T) {
	d, _ := open(t).Digest(indexed, 8, 400)
	if d.EntryCount != 2 {
		t.Fatalf("заглушки [Tool: …] не в счётчике, получено %d", d.EntryCount)
	}
	if len(d.Entries) != 2 {
		t.Fatalf("и не в ленте, получено %d", len(d.Entries))
	}
}

func TestLastActivityCountsToolStubs(t *testing.T) {
	// Расхождение намеренное: панель, час гоняющая инструменты, не простаивает.
	d, _ := open(t).Digest(indexed, 8, 400)
	if d.LastTS != 1788164400 {
		t.Fatalf("последняя активность учитывает заглушки, получено %d", d.LastTS)
	}
}

func TestTimestampsConvertedToSeconds(t *testing.T) {
	d, _ := open(t).Digest(indexed, 8, 400)
	if d.Entries[0].TS != 1788164338 {
		t.Fatalf("миллисекунды cass переводятся в секунды, получено %d", d.Entries[0].TS)
	}
}

func TestEntriesNewestFirst(t *testing.T) {
	d, _ := open(t).Digest(indexed, 8, 400)
	if d.Entries[0].ID != 2 || d.Entries[1].ID != 1 {
		t.Fatalf("свежие вперёд, получено %d %d", d.Entries[0].ID, d.Entries[1].ID)
	}
}

func TestLimitTrimsTimeline(t *testing.T) {
	d, _ := open(t).Digest(indexed, 1, 400)
	if len(d.Entries) != 1 {
		t.Fatalf("limit режет ленту, получено %d", len(d.Entries))
	}
}

func TestMissingSessionIsNotSilentEmpty(t *testing.T) {
	if _, err := open(t).Digest("00000000-0000-0000-0000-000000000000", 8, 400); err == nil {
		t.Fatal("несуществующая сессия должна быть ошибкой, а не пустотой")
	}
}

func TestSearchFindsCyrillic(t *testing.T) {
	// То, чего не умеет лексический индекс cass: «миграция» даёт там ноль.
	res, err := open(t).Search(`"кэшбек"`, SearchOpts{Limit: 5})
	if err != nil || len(res) != 1 {
		t.Fatalf("кириллица ищется, получено %d %v", len(res), err)
	}
	if res[0].Agent != "codex" || res[0].SessionID != cliIngested {
		t.Fatalf("попадание несёт агента и сессию, получено %+v", res[0])
	}
}

func TestSearchScopedToConversation(t *testing.T) {
	res, _ := open(t).Search(`"кэшбек"`, SearchOpts{Limit: 5, ConvID: 1})
	if len(res) != 0 {
		t.Fatalf("поиск по одной сессии не выходит за её пределы, получено %d", len(res))
	}
}

func TestSearchRespectsLimit(t *testing.T) {
	s := open(t)
	all, _ := s.Search(`"River" OR "кэшбек"`, SearchOpts{Limit: 5})
	if len(all) != 2 {
		t.Fatalf("без ограничения оба, получено %d", len(all))
	}
	one, _ := s.Search(`"River" OR "кэшбек"`, SearchOpts{Limit: 1})
	if len(one) != 1 {
		t.Fatalf("limit соблюдается, получено %d", len(one))
	}
}

func TestEntryReturnsWholeText(t *testing.T) {
	e, err := open(t).Entry(2)
	if err != nil || e.Text != "влит bonuses!874" {
		t.Fatalf("запись целиком, получено %+v %v", e, err)
	}
	if e.Agent != "claude_code" {
		t.Fatalf("запись несёт агента, получено %q", e.Agent)
	}
}

func TestEntryMissingExplains(t *testing.T) {
	if _, err := open(t).Entry(404); err == nil {
		t.Fatal("несуществующая запись объясняется, а не молчит")
	}
}

func TestContextWindowMarksAnchor(t *testing.T) {
	c, err := open(t).Context(1, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Entries[0].Anchor {
		t.Fatal("якорь помечен")
	}
	for _, e := range c.Entries[1:] {
		if e.Anchor {
			t.Fatalf("якорь один, помечена ещё и %d", e.ID)
		}
	}
}

func TestContextRespectsWindowBounds(t *testing.T) {
	c, _ := open(t).Context(2, 0, 0)
	if len(c.Entries) != 1 || c.Entries[0].ID != 2 {
		t.Fatalf("границы окна, получено %+v", c.Entries)
	}
}

func TestEmptyTargetIsRefused(t *testing.T) {
	// У панели Codex нет session_id; пустая цель не должна подобрать разговор
	// с пустым полем и выдать его за историю этой панели.
	s := open(t)
	if _, err := s.Digest("", 5, 100); err == nil {
		t.Fatal("пустая цель отклоняется, а не подбирает чужой разговор")
	}
}

func TestMatchQuotesEveryTerm(t *testing.T) {
	// Дефис и двоеточие внутри слова — синтаксис FTS5, а не буквы: без кавычек
	// «SD-6613» ищется как «SD» без «6613».
	for _, c := range []struct{ in, want string }{
		{"миграция", `"миграция"`},
		{"SD-6613 откат", `"SD-6613" "откат"`},
		{`он сказал "нет"`, `"он" "сказал" """нет"""`},
		{"  ", ""},
	} {
		if got := Match(c.in); got != c.want {
			t.Fatalf("Match(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

func TestMatchedQueryFindsHyphenatedToken(t *testing.T) {
	s := open(t)
	if hits, err := s.Search(Match("SD-6613"), SearchOpts{Limit: 5}); err != nil {
		t.Fatalf("выражение принимается движком: %v", err)
	} else {
		_ = hits
	}
}

func TestContextKeepsToolStubs(t *testing.T) {
	// В ленте заглушки инструментов — шум, а в окне вокруг записи именно они
	// и показывают, что делалось; прежняя версия их здесь сохраняла.
	s := open(t)
	w, err := s.Context(2, 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	var tools int
	for _, e := range w.Entries {
		if strings.HasPrefix(e.Text, "[Tool:") {
			tools++
		}
	}
	if tools == 0 {
		t.Fatalf("заглушки остаются в окне, записей %d", len(w.Entries))
	}
}

// Отдельная фикстура: нужно, чтобы попадания вне разговора и вне срока стояли
// в ранжировании выше нужного, иначе прежнюю ошибку не воспроизвести.
const crowded = `
create table conv (id integer primary key, agent text, session_id text,
  source_path text, workspace text, title text, started_at integer, ended_at integer);
create table msg (id integer primary key, conv_id integer, idx integer,
  role text, created_at integer, len integer, is_tool integer not null default 0);
create virtual table msg_fts using fts5(content, tokenize='unicode61 remove_diacritics 2');

insert into conv values (1,'claude_code','шумная','/p/шумная.jsonl','/g','Шум',0,0),
                        (2,'claude_code','нужная','/p/нужная.jsonl','/g','Нужная',0,0);
`

func crowdedStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("открытие: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(crowded); err != nil {
		t.Fatalf("фикстура: %v", err)
	}
	now := time.Now().UnixMilli()
	old := now - 400*24*3600*1000
	// Двадцать коротких попаданий в чужом разговоре и давно: bm25 ставит их выше.
	for i := 1; i <= 20; i++ {
		if _, err := s.db.Exec(`insert into msg values (?,1,?,'user',?,6,0)`, i, i, old); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`insert into msg_fts (rowid,content) values (?,'wallet')`, i); err != nil {
			t.Fatal(err)
		}
	}
	// Одно длинное попадание в нужном разговоре и сегодня.
	long := "wallet " + strings.Repeat("прочий текст ", 40)
	if _, err := s.db.Exec(`insert into msg values (99,2,0,'user',?,?,0)`, now, len(long)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`insert into msg_fts (rowid,content) values (99,?)`, long); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSearchInConversationLooksPastHigherRankedOutsiders(t *testing.T) {
	// Прежде отбор по разговору применялся к уже взятому окну лучших по рангу,
	// и запрос «wallet» внутри сессии с сотнями попаданий отдавал пусто.
	hits, err := crowdedStore(t).Search(`"wallet"`, SearchOpts{Limit: 1, ConvID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != 99 {
		t.Fatalf("попадание в своём разговоре, получено %+v", hits)
	}
}

func TestSearchByDaysLooksPastHigherRankedOlderHits(t *testing.T) {
	hits, err := crowdedStore(t).Search(`"wallet"`, SearchOpts{Limit: 1, Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != 99 {
		t.Fatalf("свежее попадание не должно вытесняться старыми, получено %+v", hits)
	}
}

func TestContextWindowClipsAtConversationEdges(t *testing.T) {
	// Окно шире разговора не должно ни падать, ни втягивать чужие записи.
	s := open(t)
	w, err := s.Context(1, 500, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Entries) != 3 {
		t.Fatalf("в разговоре 1 три записи, получено %d", len(w.Entries))
	}
	for i, want := range []int64{1, 2, 3} {
		if w.Entries[i].ID != want {
			t.Fatalf("порядок по времени: на месте %d запись %d, ожидалась %d", i, w.Entries[i].ID, want)
		}
	}
}

func TestContextWindowAsymmetric(t *testing.T) {
	// before и after считаются от якоря каждый по своей стороне.
	w, err := open(t).Context(2, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Entries) != 2 || w.Entries[0].ID != 1 || w.Entries[1].ID != 2 {
		t.Fatalf("одна запись до якоря и сам якорь, получено %+v", w.Entries)
	}
}
