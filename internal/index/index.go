// Package index собирает свой поисковый индекс из базы cass.
//
// Своя база, а не запись в чужую: cass владеет своей и может перестраивать её
// в любой момент. Урок Contextify — два писателя в один файл.
package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func DefaultCass() string {
	if p := os.Getenv("CLAUDEX_CASS_DB"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support",
		"com.coding-agent-search.coding-agent-search", "agent_search.db")
}

const schema = `
create table if not exists meta (k text primary key, v text);
create table if not exists conv (
  id integer primary key, agent text, session_id text, source_path text,
  workspace text, title text, started_at integer, ended_at integer
);
create index if not exists conv_session on conv(session_id);
create index if not exists conv_path on conv(source_path);
create table if not exists msg (
  id integer primary key, conv_id integer, idx integer,
  role text, created_at integer, len integer, is_tool integer not null default 0
);
create index if not exists msg_conv on msg(conv_id, is_tool, created_at desc, id desc);
create virtual table if not exists msg_fts using fts5(
  content, tokenize='unicode61 remove_diacritics 2'
);`

// Идентификатор сессии — последние 36 знаков имени файла: у Claude Code это
// весь basename, у Codex — хвост после rollout-<метка времени>-.
const sessionID = `
  case when c.source_path like '%.jsonl'
    then substr(replace(c.source_path,'.jsonl',''),
                length(replace(c.source_path,'.jsonl','')) - 35)
    else null end`

// Роли cass не совпадают с нашими: agent — это assistant. Developer и tool в
// ленте не участвуют, но выбрасывать их нельзя, иначе idx поплывёт.
const role = `case m.role when 'agent' then 'assistant' else m.role end`

// 148 тысяч записей из 215 — заглушки вызовов инструментов с ролью assistant.
// Отличить их можно только по тексту, а тянуть текст на каждом запросе дорого,
// поэтому признак считается один раз при сборке.
const isTool = `case when m.content like '[Tool:%' or m.role in ('tool','developer') then 1 else 0 end`

type Stats struct {
	Index          string `json:"index"`
	Source         string `json:"source"`
	Conversations  int    `json:"conversations"`
	Messages       int    `json:"messages"`
	Conversational int    `json:"conversational"`
	Added          int    `json:"added"`
	ElapsedMS      int64  `json:"elapsed_ms"`
}

// Build догоняет индекс до состояния базы cass. Инкрементально по отметке:
// полный проход по 215 тысячам записей ради последних двадцати не нужен.
func Build(cassPath, indexPath string, full bool) (Stats, error) {
	st := Stats{Index: indexPath, Source: cassPath}
	if _, err := os.Stat(cassPath); err != nil {
		return st, fmt.Errorf("базы cass нет по пути %s — выполните `cass index --full`", cassPath)
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return st, err
	}
	if full {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(indexPath + suffix)
		}
	}

	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		return st, err
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		return st, err
	}

	var watermark int64
	db.QueryRow(`select cast(coalesce(v,'0') as integer) from meta where k='watermark'`).Scan(&watermark)

	started := time.Now()
	if err := refresh(db, cassPath, watermark); err != nil {
		return st, err
	}
	st.ElapsedMS = time.Since(started).Milliseconds()

	for q, into := range map[string]*int{
		`select count(*) from conv`:                &st.Conversations,
		`select count(*) from msg`:                 &st.Messages,
		`select count(*) from msg where is_tool=0`: &st.Conversational,
	} {
		if err := db.QueryRow(q).Scan(into); err != nil {
			return st, err
		}
	}
	err = db.QueryRow(`select count(*) from msg where id > ?`, watermark).Scan(&st.Added)
	return st, err
}

func refresh(db *sql.DB, cassPath string, watermark int64) error {
	// Присоединяется только на чтение: перестраивать чужую базу мы не вправе.
	if _, err := db.Exec(fmt.Sprintf(`attach database 'file:%s?mode=ro' as cass`,
		strings.ReplaceAll(cassPath, "'", "''"))); err != nil {
		return fmt.Errorf("не присоединить базу cass: %w", err)
	}
	defer db.Exec(`detach database cass`)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, q := range []string{
		`insert or replace into conv (id, agent, session_id, source_path, workspace, title, started_at, ended_at)
		 select c.id, a.name, ` + sessionID + `, c.source_path, w.path, c.title, c.started_at, c.ended_at
		 from cass.conversations c
		 join cass.agents a on a.id = c.agent_id
		 left join cass.workspaces w on w.id = c.workspace_id`,

		`insert or replace into msg (id, conv_id, idx, role, created_at, len, is_tool)
		 select m.id, m.conversation_id, m.idx, ` + role + `, m.created_at, length(m.content), ` + isTool + `
		 from cass.messages m where m.id > ?`,

		`insert into msg_fts (rowid, content)
		 select m.id, m.content from cass.messages m where m.id > ?`,

		`insert or replace into meta (k, v)
		 select 'watermark', coalesce(max(id), ?) from msg`,
	} {
		if strings.Contains(q, "?") {
			if _, err := tx.Exec(q, watermark); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}
