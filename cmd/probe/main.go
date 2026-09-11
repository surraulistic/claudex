package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
)

func bench(name string, n int, fn func() error) {
	var ds []time.Duration
	for i := 0; i < n; i++ {
		t := time.Now()
		if err := fn(); err != nil {
			fmt.Printf("  %-28s ошибка: %v\n", name, err)
			return
		}
		ds = append(ds, time.Since(t))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	fmt.Printf("  %-28s мин %6.1f мс   медиана %6.1f мс   макс %6.1f мс\n",
		name, ms(ds[0]), ms(ds[len(ds)/2]), ms(ds[len(ds)-1]))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func main() {
	c := herdr.New(herdr.DefaultSocket())
	agents, err := c.Agents()
	if err != nil {
		fmt.Println(err)
		return
	}
	target := agents[0].PaneID
	var out any

	bench("agent.list", 9, func() error { _, e := c.Agents(); return e })
	bench("tab.list", 9, func() error { _, e := c.Tabs(); return e })
	bench("agent.get (одна панель)", 9, func() error { _, e := c.Get(target); return e })
	bench("agent.read visible 40", 9, func() error { _, e := c.Read(target, "visible", 40); return e })
	bench("ping", 9, func() error { return c.Call("ping", nil, &out) })
	bench("session.info", 9, func() error { return c.Call("session.info", nil, &out) })
	bench("pane.list", 9, func() error { return c.Call("pane.list", nil, &out) })
	bench("events.subscribe+close", 9, func() error {
		s, e := c.Subscribe(herdr.StatusSubs([]string{target}))
		if e == nil {
			s.Close()
		}
		return e
	})
}
