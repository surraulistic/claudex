package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D2: одна большая запись не делает нечитаемыми остальные.
func TestFiveMebibyteRecordAndEverythingAfterItAreRead(t *testing.T) {
	j := Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
	big := strings.Repeat("я", 5<<20/2+1)
	for _, r := range []Record{
		{Task: "aaaa0001", Event: Started},
		{Task: "aaaa0001", Event: Idle, Reason: big},
		{Task: "aaaa0002", Event: Started},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := j.parse()
	if err != nil || len(recs) != 3 {
		t.Fatalf("все три записи прочитаны без ошибки: %d %v", len(recs), err)
	}
	if recs[1].Reason != big || recs[2].Task != "aaaa0002" {
		t.Fatal("большая запись цела, следующая за ней видна")
	}
}

func TestLineOverTheLimitIsSkippedAndNotWritten(t *testing.T) {
	prev := maxLine
	maxLine = 1 << 20
	t.Cleanup(func() { maxLine = prev })
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	j := Open(path)
	j.Append(Record{Task: "aaaa0001", Event: Started})
	if err := j.Append(Record{Task: "aaaa0001", Event: Idle, Reason: strings.Repeat("x", 2<<20)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("запись, которую чтение не возьмёт, не пишется: %v", err)
	}
	// Такая строка всё же могла попасть в файл (прежняя сборка): чтение её
	// пропускает и идёт дальше.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"task":"aaaa0001","event":"idle","reason":"` + strings.Repeat("x", 2<<20) + "\"}\n")
	f.Close()
	j.Append(Record{Task: "aaaa0002", Event: Started})
	recs, err := j.parse()
	if err != nil || len(recs) != 2 || recs[1].Task != "aaaa0002" {
		t.Fatalf("строка сверх предела пропущена, остальные прочитаны: %+v %v", recs, err)
	}
}
