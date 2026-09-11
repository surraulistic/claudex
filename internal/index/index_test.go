package index

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// fakeCass — база в той же форме, что у cass: разговоры, агенты, рабочие
// каталоги и сообщения.
func fakeCass(t *testing.T, msgs int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cass.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must(t, db, `
		create table agents (id integer primary key, name text);
		create table workspaces (id integer primary key, path text);
		create table conversations (id integer primary key, agent_id integer,
			workspace_id integer, source_path text, title text,
			started_at integer, ended_at integer);
		create table messages (id integer primary key, conversation_id integer,
			idx integer, role text, created_at integer, content text);
		insert into agents values (1,'claude_code'),(2,'codex');
		insert into workspaces values (1,'/п/казино');
		insert into conversations values
			(1,1,1,'/x/7e7dd560-896f-4d96-9618-1924a89087a3.jsonl','медиа',1000,2000),
			(2,2,1,'/y/rollout-2026-09-07T10-11-12-01a079b9-5590-7201-8d53-afc3352bf9a2.jsonl','кодекс',1000,2000);`)
	for i := 1; i <= msgs; i++ {
		role, text := "user", fmt.Sprintf("сообщение %d про миграцию", i)
		switch i % 3 {
		case 0:
			role, text = "agent", "[Tool: Bash] ls"
		case 1:
			role = "agent"
		}
		must(t, db, fmt.Sprintf(
			`insert into messages values (%d, %d, %d, '%s', %d, '%s')`,
			i, 1+i%2, i, role, 1700000000000+int64(i), text))
	}
	return path
}

func must(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

func count(t *testing.T, path, q string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestBuildFillsIndex(t *testing.T) {
	cass := fakeCass(t, 9)
	out := filepath.Join(t.TempDir(), "index.db")
	st, err := Build(cass, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Conversations != 2 || st.Messages != 9 || st.Added != 9 {
		t.Fatalf("сводка сборки, получено %+v", st)
	}
	if n := count(t, out, "select count(*) from msg_fts"); n != 9 {
		t.Fatalf("текст попал в поиск, записей %d", n)
	}
}

func TestToolStubsAreMarkedOnce(t *testing.T) {
	// 148 тысяч записей из 215 — заглушки вызовов. Признак считается при
	// сборке: тянуть текст на каждом запросе, чтобы отличить их, дорого.
	cass := fakeCass(t, 9)
	out := filepath.Join(t.TempDir(), "index.db")
	if _, err := Build(cass, out, false); err != nil {
		t.Fatal(err)
	}
	if n := count(t, out, "select count(*) from msg where is_tool = 1"); n != 3 {
		t.Fatalf("заглушки помечены, получено %d из 9", n)
	}
}

func TestSecondRunOnlyAddsNew(t *testing.T) {
	// Инкрементально по отметке: полный проход по 215 тысячам записей ради
	// последних двадцати не нужен.
	cass := fakeCass(t, 9)
	out := filepath.Join(t.TempDir(), "index.db")
	if _, err := Build(cass, out, false); err != nil {
		t.Fatal(err)
	}
	st, err := Build(cass, out, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Added != 0 {
		t.Fatalf("нового нет — добавлять нечего, получено %d", st.Added)
	}
	if st.Messages != 9 {
		t.Fatalf("и ничего не задвоилось, записей %d", st.Messages)
	}
}

func TestSessionIDIsTakenFromFileName(t *testing.T) {
	// У Claude Code имя файла — это весь идентификатор, у Codex он спрятан в
	// хвосте после rollout-<метка времени>-.
	cass := fakeCass(t, 3)
	out := filepath.Join(t.TempDir(), "index.db")
	if _, err := Build(cass, out, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"7e7dd560-896f-4d96-9618-1924a89087a3",
		"01a079b9-5590-7201-8d53-afc3352bf9a2",
	} {
		if n := count(t, out, fmt.Sprintf("select count(*) from conv where session_id = '%s'", want)); n != 1 {
			t.Fatalf("идентификатор %s не выделен из имени файла", want)
		}
	}
}

func TestFullRebuildStartsClean(t *testing.T) {
	cass := fakeCass(t, 9)
	out := filepath.Join(t.TempDir(), "index.db")
	if _, err := Build(cass, out, false); err != nil {
		t.Fatal(err)
	}
	st, err := Build(cass, out, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Added != 9 || st.Messages != 9 {
		t.Fatalf("полная пересборка берёт всё заново, получено %+v", st)
	}
}

func TestMissingCassSaysWhatToRun(t *testing.T) {
	_, err := Build(filepath.Join(t.TempDir(), "нет.db"), filepath.Join(t.TempDir(), "i.db"), false)
	if err == nil {
		t.Fatal("отсутствующая база — ошибка")
	}
	if !contains(err.Error(), "cass index") {
		t.Fatalf("в отказе сказано, что запустить, получено %q", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
