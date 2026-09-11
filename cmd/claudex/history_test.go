package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/freshness"
	"github.com/surraulistic/claudex/internal/store"

	_ "modernc.org/sqlite"
)

// Воспроизведение сбоя, о котором пришёл отчёт: поручение на панели install
// завершилось в 16:52, транскрипт дописан к 17:59, но cass до него не дошёл —
// и `claudex install` отдавал историю на 16:52, ничем не выдавая, что она
// отстала на час.
func indexWithStaleHistory(t *testing.T, indexed, inFile time.Time) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	transcript := filepath.Join(dir, "7e403273.jsonl")

	var body []byte
	for _, ts := range []time.Time{inFile.Add(-time.Minute), inFile} {
		line, _ := json.Marshal(map[string]string{"type": "assistant", "timestamp": ts.UTC().Format(time.RFC3339)})
		body = append(body, append(line, '\n')...)
	}
	if err := os.WriteFile(transcript, body, 0o644); err != nil {
		t.Fatal(err)
	}
	// Файл дописан позже последней проиндексированной записи — иначе читать
	// его незачем и признак не сработает.
	if err := os.Chtimes(transcript, inFile, inFile); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "index.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		create table conv (id integer primary key, agent text, session_id text,
			source_path text, workspace text, title text, started_at integer, ended_at integer);
		create table msg (id integer primary key, conv_id integer, idx integer,
			role text, created_at integer, len integer, is_tool integer not null default 0);
		create virtual table msg_fts using fts5(content, tokenize='unicode61 remove_diacritics 2');`); err != nil {
		t.Fatal(err)
	}
	ms := indexed.UnixMilli()
	db.Exec(`insert into conv values (1568,'claude_code','7e403273',?,'/п','з',?,?)`, transcript, ms, ms)
	db.Exec(`insert into msg values (1,1568,0,'user',?,10,0),(2,1568,1,'assistant',?,20,0)`, ms-1000, ms)
	db.Exec(`insert into msg_fts (rowid, content) values (1,'вопрос'),(2,'ответ')`)
	db.Close()

	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, transcript
}

func TestDigestMarksHistoryThatLagsBehindTheTranscript(t *testing.T) {
	indexed := time.Now().Add(-2 * time.Hour)
	inFile := indexed.Add(time.Hour)
	db, _ := indexWithStaleHistory(t, indexed, inFile)

	h, _, err := digestHistory(db, "7e403273", opts{limit: 8, chars: 400}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if h.LastActivity == nil || *h.LastActivity != indexed.Format(time.RFC3339) {
		t.Fatalf("история осталась прежней, получено %v", h.LastActivity)
	}
	if h.Stale == nil {
		t.Fatal("история отстала на час и обязана об этом сказать")
	}
	if h.Stale.Cause != freshness.CauseSourceBehind {
		t.Fatalf("причина названа машиночитаемо, получено %q", h.Stale.Cause)
	}
	if h.Stale.BehindSeconds < 3500 {
		t.Fatalf("отставание измерено, получено %d с", h.Stale.BehindSeconds)
	}
	if h.Stale.ObservedAt.IsZero() || h.Stale.Reason == "" {
		t.Fatalf("момент наблюдения и причина — часть договора, получено %+v", h.Stale)
	}
	if h.Stale.SourceNewest == nil {
		t.Fatal("свидетельство названо: время записи, найденной в транскрипте")
	}
}

func TestFinishedTaskNewerThanHistoryIsReported(t *testing.T) {
	// Журнал поручений остаётся достоверным источником завершения. Если он
	// говорит, что работа кончилась позже последней записи истории, историю
	// нельзя выдавать за текущую — даже когда транскрипт недоступен.
	indexed := time.Now().Add(-2 * time.Hour)
	db, transcript := indexWithStaleHistory(t, indexed, indexed)
	os.Remove(transcript)

	done := indexed.Add(40 * time.Minute)
	h, _, err := digestHistory(db, "7e403273", opts{limit: 8, chars: 400}, done)
	if err != nil {
		t.Fatal(err)
	}
	if h.Stale == nil || h.Stale.Cause != freshness.CauseJournalAhead {
		t.Fatalf("журнал опережает историю, получено %+v", h.Stale)
	}
	if h.Stale.SourceNewest != nil {
		t.Fatal("транскрипта нет — выдумывать свидетельство нельзя")
	}
}

func TestFreshHistoryCarriesNoStaleMark(t *testing.T) {
	// Отсутствие метки значит «сверено и свежо», а не «не проверяли».
	now := time.Now()
	db, _ := indexWithStaleHistory(t, now, now)
	h, _, err := digestHistory(db, "7e403273", opts{limit: 8, chars: 400}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Stale != nil {
		t.Fatalf("свежая история не помечается, получено %+v", h.Stale)
	}
	raw, _ := json.Marshal(h)
	if got := string(raw); jsonHas(got, "stale") {
		t.Fatalf("в выдаче нет пустого поля stale: %s", got)
	}
}

func jsonHas(s, key string) bool {
	var m map[string]any
	json.Unmarshal([]byte(s), &m)
	_, ok := m[key]
	return ok
}

var _ = fmt.Sprintf
