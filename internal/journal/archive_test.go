package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

func TestTooSmallAgeIsRefusedNotObeyed(t *testing.T) {
	// Срок архивации задавался в двух местах разными единицами: команда
	// понимала дни, флаг наблюдателя — секунды. `--archive-after 30` означало
	// «старше тридцати секунд», и журнал уехал в архив целиком: 1457 записей,
	// 461 поручение. Осмысленной архивации на сроке меньше суток не бывает.
	path := withTasks(t, map[string]time.Duration{"aaaaaaaa": 60 * 24 * time.Hour})
	before, _ := os.ReadFile(path)

	_, err := Archive(path, 30*time.Second, false)
	if !errors.Is(err, ErrArchiveTooSoon) {
		t.Fatalf("срок меньше суток — отказ, получено %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("и журнал не тронут")
	}
}

func TestADayIsStillAllowed(t *testing.T) {
	// Граница проходима: сутки — законный срок.
	path := withTasks(t, map[string]time.Duration{"aaaaaaaa": 60 * 24 * time.Hour})
	if _, err := Archive(path, MinArchiveAge, true); err != nil {
		t.Fatalf("сутки принимаются, получено %v", err)
	}
}

func TestAppendWaitsWhileArchiveHoldsTheJournal(t *testing.T) {
	// R6: между чтением и rename дописанное в старый файл пропало бы.
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	done := make(chan error, 1)
	release := make(chan struct{})
	held := make(chan struct{})
	go withLock(path, syscall.LOCK_EX, func() error {
		close(held)
		<-release
		return nil
	})
	<-held
	go func() { done <- Open(path).Append(Record{Task: "cccccccc", Event: Started}) }()
	select {
	case err := <-done:
		t.Fatalf("запись прошла мимо замка архивации: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRecordsAppendedDuringArchiveSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	j := Open(path)
	old := time.Now().Add(-60 * 24 * time.Hour)
	for i := 0; i < 5000; i++ {
		j.Append(Record{Task: fmt.Sprintf("%08x", i), Event: Started, Time: old, Prompt: strings.Repeat("x", 200)})
	}
	// Писатели дописывают всё время, пока идёт архивация: окно между чтением
	// и rename они застают наверняка.
	const writers = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	stop := make(chan struct{})
	written := 0
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if Open(path).Append(Record{Task: fmt.Sprintf("f%03x%04x", w, i), Event: Started}) == nil {
					mu.Lock()
					written++
					mu.Unlock()
				}
			}
		}(w)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := Archive(path, 30*24*time.Hour, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
	recs, err := Open(path).Read()
	if err != nil {
		t.Fatal(err)
	}
	fresh := 0
	for _, r := range recs {
		if strings.HasPrefix(r.Task, "f") {
			fresh++
		}
	}
	if fresh != written {
		t.Fatalf("дописанное во время архивации цело: %d из %d", fresh, written)
	}
}
