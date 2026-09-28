package journal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Journal {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
}

func TestAppendAndRead(t *testing.T) {
	j := open(t)
	j.Append(Record{Task: "т1", Event: Started, Pane: "wE:p1"})
	j.Append(Record{Task: "т1", Event: Finished, Outcome: "готово"})
	recs, err := j.Read()
	if err != nil || len(recs) != 2 {
		t.Fatalf("две записи, получено %d %v", len(recs), err)
	}
	if recs[0].Event != Started || recs[1].Outcome != "готово" {
		t.Fatalf("порядок и поля, получено %+v", recs)
	}
	if recs[0].Time.IsZero() {
		t.Fatal("время проставляется само")
	}
}

func TestConcurrentAppendsKeepLinesWhole(t *testing.T) {
	// Отчёт пишет чужой процесс из панели, пока ожидающий читает. Строки не
	// должны перемешаться, иначе сцепка задачи с ответом рассыпается.
	j := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j.Append(Record{Task: "т", Event: Reported, Reason: strings.Repeat("я", 200)})
		}(i)
	}
	wg.Wait()
	recs, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 50 {
		t.Fatalf("пятьдесят целых строк, получено %d", len(recs))
	}
	for _, r := range recs {
		if len([]rune(r.Reason)) != 200 {
			t.Fatalf("строка обрезана: %d знаков", len([]rune(r.Reason)))
		}
	}
}

func TestAwaitSeesReportWrittenBeforeItStarted(t *testing.T) {
	// Задача может отчитаться быстрее, чем ожидающий начнёт слушать. Именно
	// сюда проваливается делегирование, если смотреть только на новые строки.
	j := open(t)
	j.Append(Record{Task: "т1", Event: Reported, Pane: "wE:p1"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := j.Await(ctx, "т1", Reported)
	if err != nil || got.Pane != "wE:p1" {
		t.Fatalf("прошлый отчёт виден, получено %+v %v", got, err)
	}
}

func TestAwaitSeesLaterReport(t *testing.T) {
	j := open(t)
	go func() {
		time.Sleep(80 * time.Millisecond)
		j.Append(Record{Task: "т1", Event: Reported, Pane: "wE:p1", Outcome: "готово"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := j.Await(ctx, "т1", Reported)
	if err != nil || got.Outcome != "готово" {
		t.Fatalf("новый отчёт дождались, получено %+v %v", got, err)
	}
}

func TestAwaitIgnoresOtherTasks(t *testing.T) {
	j := open(t)
	j.Append(Record{Task: "чужая", Event: Reported, Outcome: "готово"})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := j.Await(ctx, "т1", Reported); err == nil {
		t.Fatal("чужой отчёт не считается своим")
	}
}

func TestAwaitStopsOnContext(t *testing.T) {
	j := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := j.Await(ctx, "т1", Reported); err == nil {
		t.Fatal("по сроку возвращается причина")
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatal("и возвращается вовремя")
	}
}

func TestBrokenLineDoesNotHideTheRest(t *testing.T) {
	// Журнал переживает обрыв записи на падении: одна испорченная строка не
	// должна прятать всё, что записано после неё.
	j := open(t)
	j.Append(Record{Task: "т1", Event: Started})
	os.WriteFile(j.Path, append(mustRead(t, j.Path), []byte("{не json\n")...), 0o644)
	j.Append(Record{Task: "т1", Event: Finished, Outcome: "готово"})

	recs, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[1].Outcome != "готово" {
		t.Fatalf("целые записи читаются, получено %+v", recs)
	}
}

func TestMissingFileIsEmptyNotError(t *testing.T) {
	j := Open(filepath.Join(t.TempDir(), "нет", "tasks.jsonl"))
	recs, err := j.Read()
	if err != nil || len(recs) != 0 {
		t.Fatalf("пустой журнал — не ошибка, получено %d %v", len(recs), err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseCount() int {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return parses
}

func TestJournalIsParsedOncePerChange(t *testing.T) {
	// Одна команда claudex читала журнал до девяти раз подряд — девять
	// разборов двух мегабайт ради одних и тех же данных.
	j := Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	if err := j.Append(Record{Task: "aaaaaaaa", Event: Started}); err != nil {
		t.Fatal(err)
	}
	before := parseCount()
	for i := 0; i < 5; i++ {
		if _, err := j.Read(); err != nil {
			t.Fatal(err)
		}
	}
	if got := parseCount() - before; got > 1 {
		t.Fatalf("пять чтений — один разбор, получено %d", got)
	}
}

func TestOutsideAppendIsNoticed(t *testing.T) {
	// Журнал дописывают и другие процессы: наблюдатель, соседний claudex, хук
	// из чужой сессии. Отдать им вчерашнюю копию значит потерять их работу.
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	j := Open(path)
	j.Append(Record{Task: "aaaaaaaa", Event: Started})
	if recs, _ := j.Read(); len(recs) != 1 {
		t.Fatalf("одна запись, получено %d", len(recs))
	}

	// Пишем в обход — так это делает другой процесс.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"task":"bbbbbbbb","event":"started","time":"2026-09-28T00:00:00Z"}` + "\n")
	f.Close()

	recs, err := j.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("чужую запись увидели, получено %d", len(recs))
	}
}

func TestOwnAppendIsVisibleImmediately(t *testing.T) {
	j := Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	j.Append(Record{Task: "aaaaaaaa", Event: Started})
	j.Read()
	j.Append(Record{Task: "aaaaaaaa", Event: Reported, Outcome: "готово"})
	recs, _ := j.Read()
	if len(recs) != 2 {
		t.Fatalf("своя запись видна сразу, получено %d", len(recs))
	}
}
