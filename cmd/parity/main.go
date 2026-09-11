package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/surraulistic/claudex/internal/store"
	_ "modernc.org/sqlite"
)

func main() {
	s, err := store.Open(os.Getenv("HOME") + "/.claudex/index.db")
	if err != nil {
		fmt.Println("открытие:", err)
		return
	}
	defer s.Close()

	t := time.Now()
	hits, err := s.Search(`"миграция" AND "River"`, store.SearchOpts{Limit: 6})
	fmt.Printf("поиск «миграция AND River»: %d попаданий за %.1f мс (err=%v)\n",
		len(hits), float64(time.Since(t).Microseconds())/1000, err)
	for _, h := range hits[:min(3, len(hits))] {
		fmt.Printf("  conv=%d agent=%s %.60s\n", h.ConvID, h.Agent, h.Text)
	}

	t = time.Now()
	d, err := s.Digest(os.Args[1], 3, 400)
	fmt.Printf("дайджест %s: conv=%d agent=%s записей=%d за %.1f мс (err=%v)\n",
		os.Args[1], d.ConvID, d.Agent, d.EntryCount, float64(time.Since(t).Microseconds())/1000, err)
	for _, e := range d.Entries {
		fmt.Printf("  %-9s %.58s\n", e.Kind, e.Text)
	}
	b, _ := json.Marshal(d.Entries[0])
	fmt.Println("форма записи:", string(b)[:min(120, len(string(b)))])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
