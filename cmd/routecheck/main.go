// Ответ на один вопрос: напишет ли claudex отчёт в эту панель прямо сейчас и
// почему. Правило отказа должно быть проверяемо на живом журнале, а не только
// в тестах — именно оно решает, попадёт ли чужой отчёт в чужой разговор.
//
//	go run ./cmd/routecheck [панель…]
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

func main() {
	home, _ := os.UserHomeDir()
	j := journal.Open(filepath.Join(home, ".claudex", "tasks.jsonl"))
	c := herdr.New(herdr.DefaultSocket())

	panes := os.Args[1:]
	if len(panes) == 0 {
		agents, err := c.Agents()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, a := range agents {
			panes = append(panes, a.PaneID)
		}
	}

	for _, p := range panes {
		a, err := c.Get(p)
		if err != nil {
			fmt.Printf("  %-8s herdr: %v\n", p, err)
			continue
		}
		have := a.Session.Value
		shared := task.SharedPane(j, p)
		v := task.MayWrite(p, have, have, shared)
		if v.OK() {
			fmt.Printf("  %-8s %-7s напишем\n", p, a.Kind)
			continue
		}
		fmt.Printf("  %-8s %-7s откажем: %s\n           %s\n", p, a.Kind, v.Cause(), v.Reason())
	}
}
