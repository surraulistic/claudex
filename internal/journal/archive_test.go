package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withTasks(t *testing.T, spec map[string]time.Duration) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	j := Open(path)
	now := time.Now()
	for id, ago := range spec {
		j.Append(Record{Task: id, Event: Started, Time: now.Add(-ago)})
		j.Append(Record{Task: id, Event: Reported, Outcome: "готово", Time: now.Add(-ago)})
	}
	return path
}

func TestOldTasksMoveOutOfTheWorkingJournal(t *testing.T) {
	path := withTasks(t, map[string]time.Duration{
		"aaaaaaaa": 60 * 24 * time.Hour,
		"bbbbbbbb": time.Hour,
	})
	res, err := Archive(path, 30*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tasks != 1 || res.Moved != 2 || res.Kept != 2 {
		t.Fatalf("уехало одно поручение целиком, получено %+v", res)
	}
	recs, _ := Open(path).Read()
	for _, r := range recs {
		if r.Task == "aaaaaaaa" {
			t.Fatal("старое из журнала ушло")
		}
	}
	arch, err := Open(res.Path).Read()
	if err != nil || len(arch) != 2 {
		t.Fatalf("и легло в архив целиком, получено %d (%v)", len(arch), err)
	}
}

func TestATaskMovesWholeOrNotAtAll(t *testing.T) {
	// Половина записей в журнале, половина в архиве — разорванная история, по
	// которой уже не разобрать, что было.
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	j := Open(path)
	now := time.Now()
	j.Append(Record{Task: "aaaaaaaa", Event: Started, Time: now.Add(-60 * 24 * time.Hour)})
	j.Append(Record{Task: "aaaaaaaa", Event: Reported, Outcome: "готово", Time: now.Add(-time.Hour)})

	res, err := Archive(path, 30*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 0 {
		t.Fatalf("по последней записи поручение живое — не трогаем, получено %+v", res)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	path := withTasks(t, map[string]time.Duration{"aaaaaaaa": 60 * 24 * time.Hour})
	before, _ := os.ReadFile(path)
	res, err := Archive(path, 30*24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Moved != 2 {
		t.Fatalf("посчитали, получено %+v", res)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("но ничего не тронули")
	}
	if res.Path != "" {
		t.Fatalf("и архива не создали, получено %q", res.Path)
	}
}

func TestArchiveIsWrittenBeforeTheJournalIsCut(t *testing.T) {
	// Обратный порядок теряет записи на первом же сбое между двумя
	// действиями. Проверяем по факту: после архивации сумма записей сходится.
	path := withTasks(t, map[string]time.Duration{
		"aaaaaaaa": 60 * 24 * time.Hour,
		"bbbbbbbb": 90 * 24 * time.Hour,
		"cccccccc": time.Hour,
	})
	res, err := Archive(path, 30*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := Open(path).Read()
	moved, _ := Open(res.Path).Read()
	if len(kept)+len(moved) != 6 {
		t.Fatalf("ни одна запись не потерялась: %d + %d", len(kept), len(moved))
	}
	if res.Moved != len(moved) || res.Kept != len(kept) {
		t.Fatalf("отчёт сходится с делом, получено %+v", res)
	}
}
