package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkParse — разбор журнала той же формы, что живой: поручение с
// промптом, отчёт, отметки. CLAUDEX_BENCH_JOURNAL — разобрать копию реального.
func BenchmarkParse(b *testing.B) {
	path := os.Getenv("CLAUDEX_BENCH_JOURNAL")
	if path == "" {
		path = filepath.Join(b.TempDir(), "tasks.jsonl")
		j := Open(path)
		for i := 0; i < 1400; i++ {
			id := fmt.Sprintf("%08x", i)
			j.Append(Record{Task: id, Event: Started, Prompt: strings.Repeat("поручение ", 100)})
			j.Append(Record{Task: id, Event: Reported, Outcome: "готово", Reason: strings.Repeat("отчёт ", 140)})
			j.Append(Record{Task: id, Event: Notified, Outcome: "разбужен"})
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(fi.Size())
	j := Open(path)
	for i := 0; i < b.N; i++ {
		if _, err := j.parse(); err != nil {
			b.Fatal(err)
		}
	}
}
