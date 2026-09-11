// Команда claudex: сводка по живым панелям herdr и по истории разговоров,
// плюс поручение задачи чужой панели с ожиданием её конца.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/state"
	"github.com/surraulistic/claudex/internal/store"
	"github.com/surraulistic/claudex/internal/target"
	"github.com/surraulistic/claudex/internal/task"
	"github.com/surraulistic/claudex/internal/textual"
)

// Код 6 отличает «панель занята» от настоящего сбоя: вызывающий может
// подождать и повторить, а не считать поручение проваленным.
const exitBusy = 6

type opts struct {
	db        string
	limit     int
	chars     int
	tailLines int
	cwd       string
	days      int
	timeout   time.Duration
	noWait    bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "claudex:", err)
		if errors.Is(err, task.ErrBusy) {
			os.Exit(exitBusy)
		}
		os.Exit(1)
	}
}

func run() error {
	var o opts
	fs := flag.NewFlagSet("claudex", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = usage
	fs.StringVar(&o.db, "db-path", defaultIndex(), "индекс claudex")
	fs.IntVar(&o.limit, "limit", 8, "записей истории или результатов поиска")
	fs.IntVar(&o.chars, "chars", 400, "символов на запись")
	fs.IntVar(&o.tailLines, "tail-lines", 12, "строк живого хвоста")
	fs.StringVar(&o.cwd, "cwd", "", "фильтр по рабочему каталогу")
	fs.IntVar(&o.days, "days", 0, "find: не старше N дней")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Minute, "watch/delegate: срок ожидания")
	fs.BoolVar(&o.noWait, "no-wait", false, "delegate: отправить и выйти")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) == 0 {
		usage()
		return nil
	}

	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "sessions":
		return cmdSessions(o, false)
	case "brief":
		return cmdSessions(o, true)
	case "find":
		return cmdFind(o, strings.Join(args[1:], " "))
	case "search":
		if len(args) < 3 {
			return fmt.Errorf("нужны цель и запрос")
		}
		return cmdSearch(o, args[1], strings.Join(args[2:], " "))
	case "entry":
		return cmdEntry(o, args[1:])
	case "context":
		return cmdContext(o, args[1:])
	case "watch":
		return cmdWatch(o, args[1:])
	case "delegate":
		if len(args) < 3 {
			return fmt.Errorf("нужны цель и задача")
		}
		return cmdDelegate(o, args[1], strings.Join(args[2:], " "))
	case "done":
		if len(args) < 2 {
			return fmt.Errorf("нужен идентификатор задачи")
		}
		return cmdDone(args[1], strings.Join(args[2:], " "))
	case "tasks":
		return cmdTasks()
	default:
		return cmdDigest(o, args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `claudex — сводка по сессиям Claude Code (cass + herdr)

  claudex sessions                       какие панели живы и что у них с историей
  claudex brief                          все панели разом: хвост, сигналы, история
  claudex <цель>                         полный дайджест одной панели
  claudex search <цель> "<запрос>"       поиск внутри одной сессии
  claudex find "<запрос>"                поиск по всем транскриптам, включая закрытые
  claudex entry <id>                     запись целиком, без обрезки
  claudex context <id>                   разговор вокруг записи
  claudex watch <цель>                   дождаться, пока панель освободится
  claudex delegate <цель> "<задача>"     поручить и дождаться, одной командой
  claudex done <id> "<что вышло>"        отчитаться о порученной задаче
  claudex tasks                          журнал поручений

Цель — pane_id, session_id, кусок заголовка или рабочий каталог.
Выход 6 означает «панель занята», а не сбой.
`)
}

func defaultIndex() string {
	if p := os.Getenv("CLAUDEX_INDEX"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudex", "index.db")
}

func defaultJournal() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudex", "tasks.jsonl")
}

func client() *herdr.Client { return herdr.New(herdr.DefaultSocket()) }

func livePanes(o opts) ([]state.Pane, error) {
	agents, err := client().Agents()
	if err != nil {
		return nil, err
	}
	st := state.New()
	st.Load(agents)
	panes := st.Panes()
	if o.cwd == "" {
		return panes, nil
	}
	var out []state.Pane
	for _, p := range panes {
		if strings.HasPrefix(p.CWD, o.cwd) {
			out = append(out, p)
		}
	}
	return out, nil
}

// На работающей панели recent отдаёт пусто — измерено на всех working-панелях,
// тогда как visible исправно даёт экран.
func rawTail(c *herdr.Client, p state.Pane, lines int) string {
	order := []string{"recent", "visible"}
	if p.Status == "working" {
		order = []string{"visible", "recent"}
	}
	want := lines * 4
	if want > herdr.CheapLines {
		want = herdr.CheapLines
	}
	for _, src := range order {
		if raw, err := c.Read(p.ID, src, want); err == nil && strings.TrimSpace(raw) != "" {
			return raw
		}
	}
	return ""
}

func tailFor(c *herdr.Client, p state.Pane, lines int) []string {
	return textual.CleanTail(rawTail(c, p, lines), lines)
}

// paneView — то, что разбирает вызывающий. Поля повторяют выдачу прежней
// версии: по ним уже написаны чужие разборщики.
type paneView struct {
	Target        string         `json:"target"`
	Label         string         `json:"label"`
	Kind          string         `json:"kind"`
	Alias         *string        `json:"alias"`
	PaneID        string         `json:"pane_id"`
	ContextPct    *int           `json:"context_pct"`
	Limits        map[string]int `json:"limits"`
	Watched       bool           `json:"watched"`
	SessionID     *string        `json:"session_id"`
	Status        string         `json:"status"`
	Title         string         `json:"title"`
	CWD           string         `json:"cwd"`
	Focused       bool           `json:"focused"`
	TranscriptID  *int64         `json:"transcript_id"`
	EntryCount    *int           `json:"entry_count"`
	LastActivity  *string        `json:"last_activity"`
	HistoryReason string         `json:"history_reason,omitempty"`
	Tail          []string       `json:"tail,omitempty"`
}

func cmdSessions(o opts, withTail bool) error {
	panes, err := livePanes(o)
	if err != nil {
		return err
	}
	c := client()
	labels := map[string]string{}
	if tabs, err := c.Tabs(); err == nil {
		for _, t := range tabs {
			labels[t.TabID] = t.Label
		}
	}
	var db *store.Store
	if s, err := store.Open(o.db); err == nil {
		db = s
		defer db.Close()
	}
	busy := watchedPanes()

	lines := o.tailLines
	if withTail && lines == 12 {
		lines = 8
	}
	// Хвосты читаются разом: последовательно это самая долгая часть команды.
	raws := make([]string, len(panes))
	var wg sync.WaitGroup
	for i, p := range panes {
		wg.Add(1)
		go func(i int, p state.Pane) {
			defer wg.Done()
			raws[i] = rawTail(c, p, lines)
		}(i, p)
	}
	wg.Wait()

	views := make([]paneView, len(panes))
	var hwg sync.WaitGroup
	for i, p := range panes {
		g := textual.Gauges(raws[i])
		v := paneView{
			Target: firstNonEmpty(labels[p.TabID], p.ID), Label: labels[p.TabID],
			Kind: p.Kind, PaneID: p.ID, ContextPct: g.ContextPct, Limits: g.Limits,
			Watched: busy[p.ID], Status: p.Status, Title: p.Title, CWD: p.CWD,
			Focused: p.Focused,
		}
		if p.SessionID != "" {
			v.SessionID = &p.SessionID
		}
		if withTail {
			v.Tail = textual.CleanTail(raws[i], lines)
		}
		views[i] = v
		// Дайджест каждой панели — отдельный запрос к индексу; подряд их
		// десять, и это самая долгая часть после herdr.
		hwg.Add(1)
		go func(i int, p state.Pane) { defer hwg.Done(); fillHistory(&views[i], db, p) }(i, p)
	}
	hwg.Wait()
	return emit(map[string]any{"panes": views})
}

func fillHistory(v *paneView, db *store.Store, p state.Pane) {
	switch {
	case db == nil:
		v.HistoryReason = "индекс недоступен"
	case p.SessionID == "":
		v.HistoryReason = "у панели нет session_id"
	default:
		d, err := db.Digest(p.SessionID, 1, 60)
		if err != nil {
			v.HistoryReason = "сессии нет в индексе — возможно, он не пересобирался"
			return
		}
		id, n := d.ConvID, d.EntryCount
		ts := time.Unix(d.LastTS, 0).Format(time.RFC3339)
		v.TranscriptID, v.EntryCount, v.LastActivity = &id, &n, &ts
	}
}

// watchedPanes — панели, по которым поручение ещё не завершилось.
func watchedPanes() map[string]bool {
	out := map[string]bool{}
	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return out
	}
	open := map[string]string{}
	for _, r := range recs {
		switch r.Event {
		case journal.Started:
			open[r.Task] = r.Pane
		case journal.Finished:
			delete(open, r.Task)
		}
	}
	for _, pane := range open {
		out[pane] = true
	}
	return out
}

func emit(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func cmdDigest(o opts, q string) error {
	panes, err := livePanes(opts{})
	if err != nil {
		return err
	}
	key := q
	if p, err := target.Resolve(q, panes); err == nil {
		key = p.SessionID
		fmt.Printf("%s  %s  %s  %s\n\n", p.ID, p.Kind, p.Status, p.Title)
		for _, l := range tailFor(client(), p, o.tailLines) {
			fmt.Printf("  %s\n", l)
		}
		fmt.Println()
	}
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	d, err := db.Digest(key, o.limit, o.chars)
	if err != nil {
		return err
	}
	fmt.Printf("разговор %d · %s · записей %d\n", d.ConvID, d.Agent, d.EntryCount)
	for _, e := range d.Entries {
		fmt.Printf("  [%d] %-9s %s\n", e.ID, e.Kind, oneLine(e.Text))
	}
	return nil
}

func cmdFind(o opts, q string) error {
	if q == "" {
		return fmt.Errorf("нужен запрос")
	}
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	hits, err := db.Search(q, store.SearchOpts{Limit: o.limit, Days: o.days})
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("ничего не нашлось")
		return nil
	}
	for _, h := range hits {
		fmt.Printf("[%d] %s %-9s разговор %d  цель %s\n      %s\n",
			h.ID, time.Unix(h.TS, 0).Format("02.01 15:04"), h.Kind, h.ConvID,
			firstNonEmpty(h.SessionID, strconv.FormatInt(h.ConvID, 10)), oneLine(h.Text))
	}
	return nil
}

func cmdSearch(o opts, tgt, q string) error {
	panes, _ := livePanes(opts{})
	key := tgt
	if p, err := target.Resolve(tgt, panes); err == nil && p.SessionID != "" {
		key = p.SessionID
	}
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	d, err := db.Digest(key, 1, 1)
	if err != nil {
		return err
	}
	hits, err := db.Search(q, store.SearchOpts{Limit: o.limit, ConvID: d.ConvID})
	if err != nil {
		return err
	}
	for _, h := range hits {
		fmt.Printf("[%d] %s %-9s %s\n", h.ID, time.Unix(h.TS, 0).Format("02.01 15:04"), h.Kind, oneLine(h.Text))
	}
	return nil
}

func cmdEntry(o opts, args []string) error {
	id, err := oneID(args)
	if err != nil {
		return err
	}
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	e, err := db.Entry(id)
	if err != nil {
		return err
	}
	fmt.Printf("запись %d · %s · %s · разговор %d\n\n%s\n",
		e.ID, e.Kind, time.Unix(e.TS, 0).Format("02.01.2006 15:04"), e.ConvID, e.Text)
	return nil
}

func cmdContext(o opts, args []string) error {
	id, err := oneID(args)
	if err != nil {
		return err
	}
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	w, err := db.Context(id, o.limit, o.limit)
	if err != nil {
		return err
	}
	for _, e := range w.Entries {
		mark := "  "
		if e.ID == w.Anchor {
			mark = "▸ "
		}
		fmt.Printf("%s[%d] %-9s %s\n", mark, e.ID, e.Kind, cut(e.Text, o.chars))
	}
	return nil
}

func cmdWatch(o opts, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("нужна цель")
	}
	panes, err := livePanes(opts{})
	if err != nil {
		return err
	}
	p, err := target.Resolve(args[0], panes)
	if err != nil {
		return err
	}
	a, err := client().Wait(p.ID, []string{"idle", "done", "blocked"}, o.timeout)
	if err != nil {
		return err
	}
	fmt.Printf("%s освободилась: %s\n", p.ID, a.Status)
	return nil
}

func cmdDelegate(o opts, tgt, prompt string) error {
	panes, err := livePanes(opts{})
	if err != nil {
		return err
	}
	p, err := target.Resolve(tgt, panes)
	if err != nil {
		return err
	}
	j := journal.Open(defaultJournal())
	if o.noWait {
		a, err := client().Get(p.ID)
		if err != nil {
			return err
		}
		if a.Status == "working" {
			return fmt.Errorf("%w: %s", task.ErrBusy, p.ID)
		}
		_, err = client().Prompt(p.ID, prompt, nil, 0)
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := task.Delegate(ctx, task.Options{
		Client: client(), Journal: j, Pane: p.ID, Prompt: prompt, Timeout: o.timeout,
	})
	if err != nil {
		return err
	}
	fmt.Printf("задача %s · %s · %s · %s\n", res.Task, p.ID, res.Outcome, res.Duration.Round(time.Second))
	if res.Said != "" || res.Reason != "" {
		fmt.Printf("  %s %s\n", res.Said, res.Reason)
	}
	if res.Outcome == task.TimedOut {
		os.Exit(1)
	}
	return nil
}

func cmdDone(id, reason string) error {
	outcome := "готово"
	if i := strings.IndexByte(reason, ' '); i > 0 && isOutcomeWord(reason[:i]) {
		outcome, reason = reason[:i], strings.TrimSpace(reason[i+1:])
	}
	return task.Report(journal.Open(defaultJournal()), id, outcome, reason)
}

func isOutcomeWord(w string) bool {
	switch strings.ToLower(w) {
	case "готово", "провал", "частично", "заблокировано":
		return true
	}
	return false
}

func cmdTasks() error {
	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return err
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Time.Before(recs[j].Time) })
	for _, r := range recs {
		fmt.Printf("%s %-8s %-9s %-8s %s %s\n",
			r.Time.Format("02.01 15:04"), r.Task, r.Event, r.Pane, r.Outcome, oneLine(r.Reason))
	}
	return nil
}

func oneID(args []string) (int64, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("нужен номер записи")
	}
	return strconv.ParseInt(args[0], 10, 64)
}

func oneLine(s string) string { return cut(strings.Join(strings.Fields(s), " "), 160) }

func cut(s string, max int) string {
	out, _ := textual.Cut(strings.Join(strings.Fields(s), " "), max)
	return out
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
