// Package store читает локальный индекс: разговоры, записи и полнотекстовый
// поиск. Сети здесь нет — только sqlite.
package store

import (
	"database/sql"
	"fmt"
	"strings"

	// Драйвер подключается самим пакетом: раньше он был только в тестовом
	// файле, и собранная команда падала на «unknown driver».
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Entry struct {
	ID     int64
	Kind   string
	TS     int64
	Len    int
	Text   string
	Anchor bool
}

type Digest struct {
	ConvID int64
	Agent  string
	// SourcePath — путь, который записал сам cass. По нему сверяют свежесть
	// истории, не угадывая расположение транскрипта.
	SourcePath string
	EntryCount int
	LastTS     int64
	Entries    []Entry
}

// Цель приходит и как session_id из herdr, и как id разговора из выдачи find.
// Второй ключ нужен потому, что у половины строк после `contextify ingest`
// provider_session_id пуст; имя файла сессии — <session-id>.jsonl, а у Codex
// хвост rollout-<метка>-<uuid>.jsonl, поэтому сверяем по окончанию.
func (s *Store) convID(target string) (int64, error) {
	// Пустая цель совпала бы с пустым session_id и вернула чужой разговор:
	// у панелей Codex его нет вовсе.
	if target == "" {
		return 0, fmt.Errorf("цель не задана")
	}
	// Точный session_id спрашивается отдельно и попадает в индекс. В одном
	// запросе со вторым ключом он в индекс не попадал: like '%…' и cast(id as
	// text) заставляют sqlite прочесть таблицу разговоров целиком — измерено
	// 1.5–2.1 мс против 0.01, на каждую панель в sessions.
	var id int64
	err := s.db.QueryRow(`select id from conv where session_id = ?`, target).Scan(&id)
	if err != sql.ErrNoRows {
		return id, err
	}
	err = s.db.QueryRow(`
		select id from conv
		where cast(id as text) = ?1
		   or source_path like '%' || ?1 || '.jsonl'
		limit 1`, target).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("сессии %q нет в индексе — возможно, он не пересобирался", target)
	}
	return id, err
}

// Заглушки инструментов — около семи записей из десяти. Они остаются в поиске,
// но не в ленте и не в счётчике; последняя активность, наоборот, считает их:
// панель, которая час гоняет инструменты, не простаивает.
func (s *Store) Digest(target string, limit, chars int) (Digest, error) {
	id, err := s.convID(target)
	if err != nil {
		return Digest{}, err
	}
	d := Digest{ConvID: id}
	var path sql.NullString
	if err := s.db.QueryRow(`select agent, source_path from conv where id = ?`, id).
		Scan(&d.Agent, &path); err != nil {
		return d, err
	}
	d.SourcePath = path.String
	var last sql.NullInt64
	s.db.QueryRow(`select count(*) from msg where conv_id = ? and is_tool = 0`, id).Scan(&d.EntryCount)
	// Индекс по msg начинается с (conv_id, is_tool), поэтому шорткат sqlite для
	// max() работает, только если is_tool закреплён; значений у него два.
	// Без этого на беседе в 11 700 записей max стоил 1.45 мс вместо 0.02.
	s.db.QueryRow(`
		select max(t)/1000 from (
		  select max(created_at) t from msg where conv_id = ?1 and is_tool = 0
		  union all
		  select max(created_at) from msg where conv_id = ?1 and is_tool = 1)`, id).Scan(&last)
	d.LastTS = last.Int64

	rows, err := s.db.Query(`
		select m.id, m.role, m.created_at/1000, m.len,
		       substr((select content from msg_fts where rowid = m.id), 1, ?)
		from msg m where m.conv_id = ? and m.is_tool = 0
		order by m.created_at desc, m.id desc limit ?`, prefix(chars), id, limit)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Kind, &e.TS, &e.Len, &e.Text); err != nil {
			return d, err
		}
		d.Entries = append(d.Entries, e)
	}
	return d, rows.Err()
}

// Префикс берётся с запасом: схлопывание пробелов при обрезке могло бы выдать
// необрезанный текст за полный.
func prefix(chars int) int {
	if n := chars * 4; n > 4096 {
		return n
	}
	return 4096
}

type Hit struct {
	ID        int64
	Kind      string
	TS        int64
	ConvID    int64
	SessionID string
	Agent     string
	Workspace string
	Text      string
}

type SearchOpts struct {
	Limit  int
	ConvID int64
	Days   int
}

// Отбор по разговору и по сроку идёт в том же запросе, что и match: взятое
// движком окно лучших по рангу — не выборка, из которой можно потом отфильтровать.
// «wallet» внутри сессии с 421 попаданием так давал ноль результатов.
func (s *Store) Search(match string, o SearchOpts) ([]Hit, error) {
	if o.Limit <= 0 {
		o.Limit = 8
	}
	where := []string{"msg_fts match ?"}
	args := []any{match}
	if o.ConvID != 0 {
		where = append(where, "m.conv_id = ?")
		args = append(args, o.ConvID)
	}
	if o.Days > 0 {
		where = append(where, "m.created_at >= (strftime('%s','now') - ? * 86400) * 1000")
		args = append(args, o.Days)
	}
	args = append(args, o.Limit)

	rows, err := s.db.Query(`
		select m.id, m.role, m.created_at/1000, c.id, coalesce(c.session_id,''),
		       c.agent, coalesce(c.workspace,''), snippet(msg_fts,0,'','','…',24)
		from msg_fts
		join msg m on m.id = msg_fts.rowid
		join conv c on c.id = m.conv_id
		where `+strings.Join(where, " and ")+`
		order by rank limit ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Hit{}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ID, &h.Kind, &h.TS, &h.ConvID, &h.SessionID, &h.Agent, &h.Workspace, &h.Text); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

type FullEntry struct {
	Entry
	ConvID    int64
	SessionID string
	Agent     string
	Workspace string
}

func (s *Store) Entry(id int64) (FullEntry, error) {
	var e FullEntry
	err := s.db.QueryRow(`
		select m.id, m.role, m.created_at/1000, m.len,
		       (select content from msg_fts where rowid = m.id),
		       c.id, coalesce(c.session_id,''), c.agent, coalesce(c.workspace,'')
		from msg m join conv c on c.id = m.conv_id where m.id = ?`, id).
		Scan(&e.ID, &e.Kind, &e.TS, &e.Len, &e.Text, &e.ConvID, &e.SessionID, &e.Agent, &e.Workspace)
	if err == sql.ErrNoRows {
		return e, fmt.Errorf("записи %d нет в индексе", id)
	}
	return e, err
}

type Window struct {
	Anchor  int64
	ConvID  int64
	Entries []Entry
}

// Окно вокруг записи в её же разговоре, в том же порядке, что и лента.
// Заглушки вызовов инструментов здесь остаются: из ленты они выброшены как шум,
// но в окне вокруг записи ровно они и объясняют, что происходило.
func (s *Store) Context(id int64, before, after int) (Window, error) {
	w := Window{Anchor: id}
	var createdAt int64
	if err := s.db.QueryRow(`select conv_id, created_at from msg where id = ?`, id).
		Scan(&w.ConvID, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return w, fmt.Errorf("записи %d нет в индексе", id)
		}
		return w, err
	}
	// Две ограниченные выборки от якоря, а не нумерация всего разговора:
	// ради тридцати строк прежний row_number() пересчитывал все записи беседы
	// и на сессии в 11 700 записей стоил 30 мс против 7.5.
	rows, err := s.db.Query(`
		select id, role, ts, len, (select content from msg_fts where rowid = id) from (
		  select * from (
		    select m.id id, m.role role, m.created_at/1000 ts, m.len len, m.created_at ca
		    from msg m where m.conv_id = ?1
		      and (m.created_at, m.id) <= (select created_at, id from msg where id = ?2)
		    order by m.created_at desc, m.id desc limit ?3 + 1)
		  union all
		  select * from (
		    select m.id, m.role, m.created_at/1000, m.len, m.created_at
		    from msg m where m.conv_id = ?1
		      and (m.created_at, m.id) > (select created_at, id from msg where id = ?2)
		    order by m.created_at, m.id limit ?4)
		) order by ca, id`,
		w.ConvID, id, before, after)
	if err != nil {
		return w, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Kind, &e.TS, &e.Len, &e.Text); err != nil {
			return w, err
		}
		e.Anchor = e.ID == id
		w.Entries = append(w.Entries, e)
	}
	return w, rows.Err()
}

// Match превращает человеческий запрос в выражение FTS5. Каждое слово берётся
// в кавычки: иначе дефис, двоеточие или звёздочка внутри слова читаются как
// синтаксис и запрос либо падает, либо ищет не то. Кавычка внутри слова
// удваивается.
func Match(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		w = strings.ReplaceAll(w, `"`, `""`)
		if w != "" {
			terms = append(terms, `"`+w+`"`)
		}
	}
	return strings.Join(terms, " ")
}
