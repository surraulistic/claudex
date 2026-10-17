package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"time":"2026-10-17T10:00:00Z","task":"465a77fc","event":"started","prompt":"x"}` + "\n"))
	f.Add([]byte("{\"event\":\"idle\"\n{обрыв\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p := filepath.Join(t.TempDir(), "tasks.jsonl")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		recs, _ := Open(p).parse()
		for _, r := range recs {
			if r.Event == "" {
				t.Fatal("запись без события не возвращается")
			}
		}
	})
}
