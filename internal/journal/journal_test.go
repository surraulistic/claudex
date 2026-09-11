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
