// Ответ на один вопрос: напишет ли claudex отчёт в эту панель прямо сейчас и
// почему. Правило отказа должно быть проверяемо на живом журнале, а не только
// в тестах — именно оно решает, попадёт ли чужой отчёт в чужой разговор.
//
// Ничего не отправляет: только читает herdr, журнал и $CODEX_HOME.
//
//	go run ./cmd/routecheck [панель…]
//	go run ./cmd/routecheck --threads [тред…]
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

func main() {
	home, _ := os.UserHomeDir()
	j := journal.Open(filepath.Join(home, ".claudex", "tasks.jsonl"))
	c := herdr.New(herdr.DefaultSocket())

	if len(os.Args) > 1 && os.Args[1] == "--threads" {
		threads(os.Args[2:])
		return
	}

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

// threads — то же про адресацию по разговору Codex. Без доводов берёт тред
// этого процесса и все, у кого сейчас есть замок писателя.
func threads(ids []string) {
	h := codex.Home()
	fmt.Printf("  CODEX_HOME      %s\n", h)
	mine := codex.ThreadID()
	if mine == "" {
		fmt.Printf("  CODEX_THREAD_ID (не выставлен: этот процесс запустил не Codex)\n")
	} else {
		fmt.Printf("  CODEX_THREAD_ID %s\n", mine)
	}

	if len(ids) == 0 {
		if mine != "" {
			ids = append(ids, mine)
		}
		locks, _ := filepath.Glob(filepath.Join(h, "thread-writer-locks", "*.lock"))
		for _, l := range locks {
			id := filepath.Base(l)
			id = id[:len(id)-len(".lock")]
			// Каталог замков держит не только треды: там же .coordination.
			if id != mine && codex.IsThreadID(id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		fmt.Println("  живых тредов не видно")
		return
	}
	for _, id := range ids {
		state := codex.State(h, id)
		v := task.MayQueue(id, state)
		if v.OK() {
			fmt.Printf("  %s %-7s положим в очередь\n", id, state)
			continue
		}
		fmt.Printf("  %s %-7s откажем: %s\n    %s\n", id, state, v.Cause(), v.Reason())
	}
}
