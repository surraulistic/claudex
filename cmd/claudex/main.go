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
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/surraulistic/claudex/internal/exitcode"
	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/index"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/state"
	"github.com/surraulistic/claudex/internal/store"
	"github.com/surraulistic/claudex/internal/target"
	"github.com/surraulistic/claudex/internal/task"
	"github.com/surraulistic/claudex/internal/textual"
)

type opts struct {
	db         string
	limit      int
	chars      int
	tailLines  int
	cwd        string
	days       int
	before     int
	after      int
	timeoutRaw string
	timeout    time.Duration
	noWait     bool
	full       bool
	raw        bool
	pretty     bool
	notify     string
	detach     bool
	force      bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "claudex:", err)
		os.Exit(exitcode.Of(err))
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
	fs.IntVar(&o.before, "before", 10, "context: записей до якоря")
	fs.IntVar(&o.after, "after", 20, "context: записей после якоря")
	fs.StringVar(&o.timeoutRaw, "timeout", "1800", "watch/delegate: срок ожидания, секунды или 30m")
	fs.StringVar(&o.notify, "notify", "", "delegate: разбудить эту панель по завершении")
	fs.BoolVar(&o.raw, "raw", false, "запрос уходит в FTS5 как есть, без экранирования")
	fs.BoolVar(&o.pretty, "pretty", false, "JSON с отступами")
	fs.BoolVar(&o.detach, "detach", false, "delegate: отдать ожидание отдельному процессу")
	fs.BoolVar(&o.force, "force", false, "delegate: писать и в панель, ждущую решения человека")
	fs.BoolVar(&o.noWait, "no-wait", false, "delegate: отправить и выйти")
	fs.BoolVar(&o.full, "full", false, "index: пересобрать с нуля")
	// Флаги принимаются где угодно, в том числе после запроса: прежняя версия
	// так умела, и «claudex find "миграция" --limit 3» пишут именно так.
	// Разбор из стандартной библиотеки останавливается на первом позиционном
	// доводе, поэтому доводы разделяются заранее.
	flags, rest := splitArgs(os.Args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}
	d, err := exitcode.Duration(o.timeoutRaw)
	if err != nil {
		return exitcode.Wrap(exitcode.BadCall, err)
	}
	o.timeout = d

	args := rest
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
	case "index":
		return cmdIndex(o)
	case "tasks":
		return cmdTasks()
	default:
		return cmdDigest(o, args[0])
	}
}

// boolFlags — флаги без значения; у остальных следующий довод считается их
// значением, если не написан через «=».
var boolFlags = map[string]bool{
	"no-wait": true, "full": true, "raw": true, "pretty": true,
	"detach": true, "force": true, "help": true, "h": true,
}

func splitArgs(argv []string) (flags, rest []string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			rest = append(rest, argv[i+1:]...)
			return
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			flags = append(flags, a)
			continue
		}
		flags = append(flags, a)
		if !boolFlags[name] && i+1 < len(argv) {
			i++
			flags = append(flags, argv[i])
		}
	}
	return
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
  claudex index [--full]                 пересобрать индекс из базы cass
  claudex tasks                          журнал поручений

Цель — pane_id, session_id, кусок заголовка или рабочий каталог.

Флаги:
  --db-path <путь>   индекс (умолчание: $CLAUDEX_INDEX или ~/.claudex/index.db)
  --limit N          записей истории (8) или результатов поиска
  --chars N          символов на запись (400)
  --tail-lines N     строк живого хвоста (12, brief 8)
  --cwd <путь>       фильтр по рабочему каталогу
  --days N           find: не старше N дней
  --before N         context: записей до якоря (10)
  --after N          context: записей после якоря (20)
  --timeout N        watch/delegate: секунды числом либо вид 30m (1800)
  --no-wait          delegate: отправить и выйти
  --detach           delegate: отдать ожидание отдельному процессу
  --force            delegate: писать и в панель, ждущую решения человека
  --notify <цель>    delegate: разбудить эту панель по завершении
  --raw              запрос уходит в FTS5 как есть, без экранирования
  --pretty           JSON с отступами
  --full             index: пересобрать с нуля

Коды выхода: 0 успех · 2 цель не найдена · 3 herdr недоступен · 4 ошибка вызова
             5 не дождался · 6 панель занята, задание не отправлено
             7 сбой herdr при ожидании — исход неизвестен
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

// tabLabels — метки вкладок, ими панель называют люди.
func tabLabels(c *herdr.Client) map[string]string {
	out := map[string]string{}
	tabs, err := c.Tabs()
	if err != nil {
		return out
	}
	byTab := map[string]string{}
	for _, t := range tabs {
		byTab[t.TabID] = t.Label
	}
	agents, err := c.Agents()
	if err != nil {
		return out
	}
	for _, a := range agents {
		if l := byTab[a.TabID]; l != "" {
			out[a.PaneID] = l
		}
	}
	return out
}

// resolve находит панель по чему угодно, чем её называют: идентификатору,
// идентификатору сессии, имени агента, метке вкладки, заголовку, каталогу.
func resolve(q string) (state.Pane, error) {
	panes, err := livePanes(opts{})
	if err != nil {
		return state.Pane{}, err
	}
	p, err := target.Resolve(q, panes, tabLabels(client()))
	if err != nil {
		return state.Pane{}, exitcode.Wrap(exitcode.NotFound, err)
	}
	return p, nil
}

func livePanes(o opts) ([]state.Pane, error) {
	agents, err := client().Agents()
	if err != nil {
		return nil, exitcode.Wrap(exitcode.NoHerdr, err)
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
}

// brief отдаёт то же самое, но историю отдельным узлом и с живым хвостом:
// у него другой читатель — тот, кто смотрит на все панели разом.
type briefView struct {
	Target     string         `json:"target"`
	Label      string         `json:"label"`
	Kind       string         `json:"kind"`
	Alias      *string        `json:"alias"`
	PaneID     string         `json:"pane_id"`
	Status     string         `json:"status"`
	ContextPct *int           `json:"context_pct"`
	Limits     map[string]int `json:"limits"`
	Watched    bool           `json:"watched"`
	Title      string         `json:"title"`
	CWD        string         `json:"cwd"`
	Focused    bool           `json:"focused"`
	History    briefHistory   `json:"history"`
	Signals    briefSignals   `json:"signals"`
	Tail       []string       `json:"tail"` // null, когда экран пуст
}

type briefHistory struct {
	TranscriptID *int64  `json:"transcript_id"`
	EntryCount   *int    `json:"entry_count"`
	LastActivity *string `json:"last_activity"`
	Reason       string  `json:"reason,omitempty"`
}

type briefSignals struct {
	MR              []string `json:"mr"`
	Tickets         []string `json:"tickets"`
	Repo            *string  `json:"repo"`
	LastUserPrompt  *string  `json:"last_user_prompt"`
	CurrentToolCall *string  `json:"current_tool_call"`
}

func toBrief(v paneView, raw string, tail []string, entries []textual.HistoryEntry) briefView {
	sig := textual.Signals(raw, entries)
	b := briefView{
		Target: v.Target, Label: v.Label, Kind: v.Kind, Alias: v.Alias,
		PaneID: v.PaneID, Status: v.Status, ContextPct: v.ContextPct,
		Limits: v.Limits, Watched: v.Watched, Title: v.Title, CWD: v.CWD,
		Focused: v.Focused, Tail: tail,
		History: briefHistory{
			TranscriptID: v.TranscriptID, EntryCount: v.EntryCount,
			LastActivity: v.LastActivity, Reason: v.HistoryReason,
		},
		Signals: briefSignals{MR: orEmpty(sig.MRs), Tickets: orEmpty(sig.Tickets)},
	}
	for _, pair := range []struct {
		from string
		into **string
	}{
		{sig.Repo, &b.Signals.Repo},
		{sig.LastUserPrompt, &b.Signals.LastUserPrompt},
		{sig.CurrentToolCall, &b.Signals.CurrentToolCall},
	} {
		if pair.from != "" {
			val := pair.from
			*pair.into = &val
		}
	}
	return b
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
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
	if withTail {
		// У brief свой читатель и свои умолчания: короче хвост, меньше записей.
		if lines == 12 {
			lines = 8
		}
		if o.limit == 8 {
			o.limit = 4
		}
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
	entries := make([][]textual.HistoryEntry, len(panes))
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
		if p.Name != "" {
			name := p.Name
			v.Alias = &name
		}
		views[i] = v
		// Дайджест каждой панели — отдельный запрос к индексу; подряд их
		// десять, и это самая долгая часть после herdr.
		hwg.Add(1)
		go func(i int, p state.Pane) {
			defer hwg.Done()
			entries[i] = fillHistory(&views[i], db, p, o.limit, o.chars)
		}(i, p)
	}
	hwg.Wait()
	order := make([]int, len(views))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return less(views)(order[a], order[b]) })

	if !withTail {
		sorted := make([]paneView, len(views))
		for k, i := range order {
			sorted[k] = views[i]
		}
		return emit(o, map[string]any{"panes": sorted})
	}
	briefs := make([]briefView, len(views))
	for k, i := range order {
		briefs[k] = toBrief(views[i], raws[i], textual.CleanTail(raws[i], lines), entries[i])
	}
	return emit(o, map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"panes":        briefs,
	})
}

// Порядок вывода: сначала деятельные панели, внутри — по свежести истории,
// панели без истории в конец своей группы. Смотрящий читает список сверху и
// должен первым делом видеть то, что происходит сейчас.
var activeFirst = map[string]int{"working": 0, "done": 1, "idle": 2}

func less(v []paneView) func(i, j int) bool {
	return func(i, j int) bool {
		a, b := v[i], v[j]
		ra, ok := activeFirst[a.Status]
		if !ok {
			ra = 3
		}
		rb, ok := activeFirst[b.Status]
		if !ok {
			rb = 3
		}
		if ra != rb {
			return ra < rb
		}
		if a.LastActivity == nil || b.LastActivity == nil {
			if (a.LastActivity == nil) != (b.LastActivity == nil) {
				return b.LastActivity == nil
			}
			return a.PaneID < b.PaneID
		}
		if *a.LastActivity != *b.LastActivity {
			return *a.LastActivity > *b.LastActivity
		}
		return a.PaneID < b.PaneID
	}
}

// fillHistory возвращает тексты записей: по ним, а не по живому экрану,
// собираются сигналы — в истории видно, над чем панель работает, даже когда
// экран занят выводом команды.
func fillHistory(v *paneView, db *store.Store, p state.Pane, limit, chars int) []textual.HistoryEntry {
	switch {
	case db == nil:
		v.HistoryReason = "индекс недоступен"
	case p.SessionID == "":
		v.HistoryReason = "у панели нет session_id"
	default:
		d, err := db.Digest(p.SessionID, limit, chars)
		if err != nil {
			v.HistoryReason = "сессии нет в индексе — возможно, он не пересобирался"
			return nil
		}
		id, n := d.ConvID, d.EntryCount
		ts := time.Unix(d.LastTS, 0).Format(time.RFC3339)
		v.TranscriptID, v.EntryCount, v.LastActivity = &id, &n, &ts
		out := make([]textual.HistoryEntry, 0, len(d.Entries))
		for _, e := range d.Entries {
			out = append(out, textual.HistoryEntry{Kind: e.Kind, Text: e.Text})
		}
		return out
	}
	return nil
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

func emit(o opts, v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if o.pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

// match готовит запрос для FTS5. --raw отдаёт его движку как есть: изредка
// нужен его собственный синтаксис, NEAR или префиксная звёздочка.
func (o opts) match(q string) string {
	if o.raw {
		return q
	}
	return store.Match(q)
}

func cmdDigest(o opts, q string) error {
	key := q
	if p, err := resolve(q); err == nil {
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
		return exitcode.Wrap(exitcode.NotFound, err)
	}
	fmt.Printf("разговор %d · %s · записей %d\n", d.ConvID, d.Agent, d.EntryCount)
	for _, e := range d.Entries {
		fmt.Printf("  [%d] %-9s %s\n", e.ID, e.Kind, oneLine(e.Text))
	}
	return nil
}

type hitView struct {
	ID   int64  `json:"id"`
	TS   string `json:"ts"`
	Role string `json:"role"`
	Text string `json:"text"`
}

type sessionView struct {
	Target       string    `json:"target"`
	SessionID    *string   `json:"session_id"`
	TranscriptID int64     `json:"transcript_id"`
	Project      string    `json:"project"`
	CWD          string    `json:"cwd"`
	Branch       *string   `json:"branch"`
	FirstHit     string    `json:"first_hit"`
	LastHit      string    `json:"last_hit"`
	Hits         int       `json:"hits"`
	Entries      []hitView `json:"entries"`
}

// group складывает попадания по разговорам и ставит свежие первыми: при
// поиске по всей истории полезнее недавний разговор, а не самый релевантный
// по мнению движка.
func group(hits []store.Hit, chars int) []sessionView {
	order := []int64{}
	by := map[int64]*sessionView{}
	for _, h := range hits {
		ts := time.Unix(h.TS, 0).Format(time.RFC3339)
		sv, ok := by[h.ConvID]
		if !ok {
			target := h.SessionID
			if target == "" {
				target = strconv.FormatInt(h.ConvID, 10)
			}
			sv = &sessionView{
				Target: target, TranscriptID: h.ConvID,
				Project: h.Workspace, CWD: h.Workspace,
				FirstHit: ts, LastHit: ts,
			}
			if h.SessionID != "" {
				id := h.SessionID
				sv.SessionID = &id
			}
			by[h.ConvID] = sv
			order = append(order, h.ConvID)
		}
		if ts < sv.FirstHit {
			sv.FirstHit = ts
		}
		if ts > sv.LastHit {
			sv.LastHit = ts
		}
		sv.Hits++
		sv.Entries = append(sv.Entries, hitView{ID: h.ID, TS: ts, Role: h.Kind, Text: cutTo(h.Text, chars)})
	}
	out := make([]sessionView, 0, len(order))
	for _, id := range order {
		out = append(out, *by[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastHit > out[j].LastHit })
	return out
}

func cmdFind(o opts, q string) error {
	if strings.TrimSpace(q) == "" {
		return fmt.Errorf("нужен запрос")
	}
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	match := o.match(q)
	hits, err := db.Search(match, store.SearchOpts{Limit: o.limit, Days: o.days})
	if err != nil {
		return err
	}
	return emit(o, map[string]any{
		"query": q, "match": match, "hits": len(hits), "sessions": group(hits, o.chars),
	})
}

func cmdSearch(o opts, tgt, q string) error {
	key, shown := tgt, tgt
	if p, err := resolve(tgt); err == nil {
		if p.SessionID != "" {
			key = p.SessionID
		}
		shown = p.ID
		if p.Name != "" {
			shown = p.Name
		}
	}
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()
	d, err := db.Digest(key, 1, 1)
	if err != nil {
		return exitcode.Wrap(exitcode.NotFound, err)
	}
	match := o.match(q)
	hits, err := db.Search(match, store.SearchOpts{Limit: o.limit, ConvID: d.ConvID})
	if err != nil {
		return err
	}
	// Плоский список: сессия здесь одна и названа выше, группировать не по чему.
	results := make([]hitView, 0, len(hits))
	for _, h := range hits {
		results = append(results, hitView{
			ID: h.ID, TS: time.Unix(h.TS, 0).Format(time.RFC3339),
			Role: h.Kind, Text: cutTo(h.Text, o.chars),
		})
	}
	return emit(o, map[string]any{
		"target": shown, "transcript_id": d.ConvID,
		"query": q, "match": match, "results": results,
	})
}

func cutTo(s string, max int) string {
	out, _ := textual.Cut(s, max)
	return out
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
		return exitcode.Wrap(exitcode.NotFound, err)
	}
	return emit(o, entryView(e))
}

func entryView(e store.FullEntry) map[string]any {
	v := map[string]any{
		"id": e.ID, "ts": time.Unix(e.TS, 0).Format(time.RFC3339), "role": e.Kind,
		"transcript_id": e.ConvID, "agent": e.Agent,
		"project": e.Workspace, "cwd": e.Workspace, "branch": nil,
		"chars": len([]rune(e.Text)), "text": e.Text,
	}
	if e.SessionID != "" {
		v["target"] = e.SessionID
	} else {
		v["target"] = strconv.FormatInt(e.ConvID, 10)
	}
	return v
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
	w, err := db.Context(id, o.before, o.after)
	if err != nil {
		return exitcode.Wrap(exitcode.NotFound, err)
	}
	entries := make([]map[string]any, 0, len(w.Entries))
	for _, e := range w.Entries {
		entries = append(entries, map[string]any{
			"id": e.ID, "ts": time.Unix(e.TS, 0).Format(time.RFC3339), "role": e.Kind,
			// Ни обрезки, ни схлопывания переводов строк: окно вокруг записи
			// читают целиком и глазами, а --chars относится к спискам.
			"chars": e.Len, "anchor": e.ID == w.Anchor, "text": e.Text,
		})
	}
	return emit(o, map[string]any{
		// Якорь строкой: им же его и передают обратно в команду.
		"anchor": strconv.FormatInt(w.Anchor, 10), "transcript_id": w.ConvID, "entries": entries,
	})
}

func cmdWatch(o opts, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("нужна цель")
	}
	p, err := resolve(args[0])
	if err != nil {
		return err
	}
	a, err := client().Wait(p.ID, []string{"idle", "done", "blocked"}, o.timeout)
	if err != nil {
		// «Не дождались» и «сломалось во время ожидания» — разные исходы:
		// во втором случае мы попросту не знаем, чем дело кончилось.
		if herdr.CodeOf(err) == "timeout" {
			return exitcode.Wrap(exitcode.Timeout, err)
		}
		return exitcode.Wrap(exitcode.Unknown, err)
	}
	return emit(o, map[string]any{"pane": p.ID, "status": a.Status, "final_status": a.Status})
}

func cmdDelegate(o opts, tgt, prompt string) error {
	p, err := resolve(tgt)
	if err != nil {
		return err
	}
	j := journal.Open(defaultJournal())

	if o.noWait {
		a, err := client().Get(p.ID)
		if err != nil {
			return exitcode.Wrap(exitcode.NoHerdr, err)
		}
		if a.Status == "working" {
			return exitcode.Errorf(exitcode.Busy, "панель занята: %s", p.ID)
		}
		if _, err := client().Prompt(p.ID, prompt, nil, 0); err != nil {
			return exitcode.Wrap(exitcode.BadCall, err)
		}
		return emit(o, map[string]any{"pane": p.ID, "sent": true, "waited": false})
	}

	if o.detach {
		// Отсоединённый наблюдатель пишет в свой журнал; без пробуждения его
		// результат не прочтёт никто.
		if o.notify == "" {
			return exitcode.Errorf(exitcode.BadCall,
				"--detach без --notify: результат наблюдателя некому прочитать")
		}
		return detach(o, p.ID, prompt)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := task.Delegate(ctx, task.Options{
		Client: client(), Journal: j, Pane: p.ID, Prompt: prompt,
		Timeout: o.timeout, Force: o.force,
	})
	if err != nil {
		if errors.Is(err, task.ErrBusy) {
			return exitcode.Wrap(exitcode.Busy, err)
		}
		return exitcode.Wrap(exitcode.BadCall, err)
	}

	out := map[string]any{
		"task": res.Task, "pane": p.ID, "outcome": res.Outcome,
		"said": res.Said, "reason": res.Reason,
		"correlated": res.Outcome == task.Reported,
		"seconds":    int(res.Duration.Seconds()),
	}
	if o.notify != "" {
		// Итог пробуждения возвращается вызывающему, а не глохнет: иначе он
		// будет ждать зова, которого не случилось.
		text := fmt.Sprintf("Поручение %s на панели %s: %s. %s %s",
			res.Task, p.ID, res.Outcome, res.Said, res.Reason)
		if err := task.Notify(ctx, client(), o.notify, text, 5, 3*time.Second); err != nil {
			out["notified"] = map[string]any{"target": o.notify, "ok": false, "reason": err.Error()}
		} else {
			out["notified"] = map[string]any{"target": o.notify, "ok": true}
		}
	}
	if err := emit(o, out); err != nil {
		return err
	}
	if res.Outcome == task.TimedOut {
		return exitcode.Errorf(exitcode.Timeout, "задача %s не уложилась в срок", res.Task)
	}
	return nil
}

// detach отдаёт ожидание отдельному процессу и возвращает управление сразу:
// ход вызывающего не занят, а разбудит его --notify.
func detach(o opts, pane, prompt string) error {
	self, err := os.Executable()
	if err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	args := []string{"delegate", pane, prompt, "--timeout", o.timeoutRaw}
	if o.notify != "" {
		args = append(args, "--notify", o.notify)
	}
	log, err := os.OpenFile(detachLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	defer log.Close()
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
	}
	// Процесс не ждём: иначе он умрёт вместе с нами.
	go cmd.Process.Release()
	return emit(o, map[string]any{
		"pane": pane, "detached": true, "pid": cmd.Process.Pid, "log": detachLog(),
	})
}

func detachLog() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudex", "detached.log")
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

func cmdIndex(o opts) error {
	st, err := index.Build(index.DefaultCass(), o.db, o.full)
	if err != nil {
		return err
	}
	return emit(o, st)
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

func trimTo(s string, max int) string {
	out, _ := textual.Clip(s, max)
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
