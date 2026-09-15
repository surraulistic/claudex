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

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/digest"
	"github.com/surraulistic/claudex/internal/exitcode"
	"github.com/surraulistic/claudex/internal/freshness"
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
	db            string
	limit         int
	chars         int
	tailLines     int
	cwd           string
	days          int
	before        int
	after         int
	timeoutRaw    string
	timeout       time.Duration
	noWait        bool
	full          bool
	raw           bool
	pretty        bool
	notify        string
	notifyThread  string
	notifyWaitRaw string
	notifyWait    time.Duration
	detach        bool
	force         bool
	task          string
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
	fs.StringVar(&o.notifyThread, "notify-thread", "",
		"delegate: вернуть результат в этот тред Codex (умолчание — $CODEX_THREAD_ID)")
	fs.StringVar(&o.notifyWaitRaw, "notify-timeout", "1800",
		"delegate: сколько ждать, пока ведущий освободится")
	fs.BoolVar(&o.raw, "raw", false, "запрос уходит в FTS5 как есть, без экранирования")
	fs.BoolVar(&o.pretty, "pretty", false, "JSON с отступами")
	fs.BoolVar(&o.detach, "detach", false, "delegate: отдать ожидание отдельному процессу")
	fs.BoolVar(&o.force, "force", false, "delegate: писать и в панель, ждущую решения человека")
	fs.BoolVar(&o.noWait, "no-wait", false, "delegate: отправить и выйти")
	fs.BoolVar(&o.full, "full", false, "index: пересобрать с нуля")
	fs.StringVar(&o.task, "task", "", "tasks: состояние одного поручения по его идентификатору")
	// Флаги принимаются где угодно, в том числе после запроса: прежняя версия
	// так умела, и «claudex find "миграция" --limit 3» пишут именно так.
	// Разбор из стандартной библиотеки останавливается на первом позиционном
	// доводе, поэтому доводы разделяются заранее.
	flags, rest := splitArgs(os.Args[1:])
	if err := fs.Parse(flags); err != nil {
		return exitcode.Wrap(exitcode.BadCall, err)
	}
	d, err := parseTimeout(o.timeoutRaw)
	if err != nil {
		return exitcode.Wrap(exitcode.BadCall, err)
	}
	o.timeout = d
	nw, err := parseTimeout(o.notifyWaitRaw)
	if err != nil {
		return exitcode.Wrap(exitcode.BadCall, err)
	}
	o.notifyWait = nw

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
			return exitcode.Errorf(exitcode.BadCall, "нужны цель и запрос")
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
			return exitcode.Errorf(exitcode.BadCall, "нужны цель и задача")
		}
		return cmdDelegate(o, args[1], strings.Join(args[2:], " "))
	case "done":
		if len(args) < 2 {
			return exitcode.Errorf(exitcode.BadCall, "нужен идентификатор задачи")
		}
		return cmdDone(o, args[1], strings.Join(args[2:], " "))
	case "digest":
		if len(args) < 2 {
			return exitcode.Errorf(exitcode.BadCall, "нужен идентификатор задачи")
		}
		return cmdTaskDigest(o, args[1])
	case "index":
		return cmdIndex(o)
	case "tasks":
		return cmdTasks(o, args[1:])
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
		flags = append(flags, a)
		// «--limit=3» несёт значение в себе; «--limit 3» забирает следующий довод.
		if strings.ContainsRune(name, '=') || boolFlags[name] || i+1 >= len(argv) {
			continue
		}
		i++
		flags = append(flags, argv[i])
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
  claudex digest <id>                    ход работы по поручению: что делалось
  claudex index [--full]                 пересобрать индекс из базы cass
  claudex tasks [--task <id>]            журнал поручений; с --task — одно

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
  --notify-thread ID delegate: вернуть результат в этот тред Codex — адрес есть
                     сам разговор (умолчание: $CODEX_THREAD_ID)
  --notify-timeout N delegate: сколько ждать освобождения ведущего (1800);
                     не дождались — факт уходит человеку уведомлением herdr
  --raw              запрос уходит в FTS5 как есть, без экранирования
  --pretty           JSON с отступами
  --full             index: пересобрать с нуля

Коды выхода: 0 успех · 2 цель не найдена · 3 herdr недоступен · 4 ошибка вызова
             5 не дождался · 6 панель занята, задание не отправлено
             7 сбой herdr при ожидании — исход неизвестен
`)
}

// parseTimeout читает и голые секунды, и человеческий срок: прежняя версия
// принимала «1800», ломать это нельзя.
func parseTimeout(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("непонятный срок %q: нужны секунды числом или вид 30m", s)
	}
	return d, nil
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

// labelsByPane раскладывает метки вкладок по панелям: меткой панель называют
// люди, а живёт она у вкладки. Список агентов передаётся уже добытым — agent.list
// самый дорогой вызов herdr, и повторять его ради той же карты не за что.
func labelsByPane(c *herdr.Client, agents []herdr.Agent) map[string]string {
	tabs, err := c.Tabs()
	if err != nil {
		return nil
	}
	byTab := make(map[string]string, len(tabs))
	for _, t := range tabs {
		byTab[t.TabID] = t.Label
	}
	out := make(map[string]string, len(agents))
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
	c := client()
	agents, err := c.Agents()
	if err != nil {
		return state.Pane{}, exitcode.Wrap(exitcode.NoHerdr, err)
	}
	p, err := target.Resolve(q, panesOf(agents, ""), labelsByPane(c, agents))
	if err != nil {
		return state.Pane{}, exitcode.Wrap(exitcode.NotFound, err)
	}
	return p, nil
}

func panesOf(agents []herdr.Agent, cwd string) []state.Pane {
	st := state.New()
	st.Load(agents)
	panes := st.Panes()
	if cwd == "" {
		return panes
	}
	var out []state.Pane
	for _, p := range panes {
		if strings.HasPrefix(p.CWD, cwd) {
			out = append(out, p)
		}
	}
	return out
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
	Title         *string        `json:"title"`
	CWD           string         `json:"cwd"`
	Focused       bool           `json:"focused"`
	TranscriptID  *int64         `json:"transcript_id"`
	EntryCount    *int           `json:"entry_count"`
	LastActivity  *string        `json:"last_activity"`
	HistoryReason string         `json:"history_reason,omitempty"`
	Stale         *freshness.Lag `json:"history_stale,omitempty"`
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
	Title      *string        `json:"title"`
	CWD        string         `json:"cwd"`
	Focused    bool           `json:"focused"`
	History    briefHistory   `json:"history"`
	Signals    briefSignals   `json:"signals"`
	Tail       []string       `json:"tail"` // null, когда экран пуст
}

type briefHistory struct {
	TranscriptID *int64         `json:"transcript_id"`
	EntryCount   *int           `json:"entry_count"`
	LastActivity *string        `json:"last_activity"`
	Stale        *freshness.Lag `json:"stale,omitempty"`
	Reason       string         `json:"reason,omitempty"`
}

type briefSignals struct {
	MR              []string `json:"mr"`
	Tickets         []string `json:"tickets"`
	Repo            *string  `json:"repo"`
	LastUserPrompt  *string  `json:"last_user_prompt"`
	CurrentToolCall *string  `json:"current_tool_call"`
}

func signalsOf(raw string, entries []textual.HistoryEntry) briefSignals {
	sig := textual.Signals(raw, entries)
	return briefSignals{
		MR: orEmpty(sig.MRs), Tickets: orEmpty(sig.Tickets),
		Repo: orNull(sig.Repo), LastUserPrompt: orNull(sig.LastUserPrompt),
		CurrentToolCall: orNull(sig.CurrentToolCall),
	}
}

func toBrief(v paneView, raw string, tail []string, entries []textual.HistoryEntry) briefView {
	return briefView{
		Target: v.Target, Label: v.Label, Kind: v.Kind, Alias: v.Alias,
		PaneID: v.PaneID, Status: v.Status, ContextPct: v.ContextPct,
		Limits: v.Limits, Watched: v.Watched, Title: v.Title, CWD: v.CWD,
		Focused: v.Focused, Tail: tail,
		History: briefHistory{
			TranscriptID: v.TranscriptID, EntryCount: v.EntryCount,
			LastActivity: v.LastActivity, Stale: v.Stale, Reason: v.HistoryReason,
		},
		Signals: signalsOf(raw, entries),
	}
}

func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// orNull: пустая строка в этих полях значит «нечего показать», и уходить в
// JSON должна как null, а не как "".
func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func cmdSessions(o opts, withTail bool) error {
	c := client()
	agents, err := c.Agents()
	if err != nil {
		return exitcode.Wrap(exitcode.NoHerdr, err)
	}
	panes := panesOf(agents, o.cwd)
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
	jf := readJournal()

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
	seen := make([]sighting, len(panes))
	var wg sync.WaitGroup
	for i, p := range panes {
		wg.Add(1)
		go func(i int, p state.Pane) {
			defer wg.Done()
			seen[i].raw = rawTail(c, p, lines)
		}(i, p)
	}
	wg.Wait()

	for i, p := range panes {
		g := textual.Gauges(seen[i].raw)
		seen[i].view = paneView{
			Target: firstNonEmpty(labels[p.TabID], p.ID), Label: labels[p.TabID],
			Kind: p.Kind, PaneID: p.ID, ContextPct: g.ContextPct, Limits: g.Limits,
			Watched: jf.watched[p.ID], Status: p.Status, Title: orNull(p.Title), CWD: p.CWD,
			Focused: p.Focused, SessionID: orNull(p.SessionID), Alias: orNull(p.Name),
		}
		// Дайджест каждой панели — отдельный запрос к индексу; подряд их
		// десять, и это самая долгая часть после herdr.
		wg.Add(1)
		go func(i int, p state.Pane) {
			defer wg.Done()
			seen[i].entries = fillHistory(&seen[i].view, db, p, o.limit, o.chars, jf.lastDone[p.ID])
		}(i, p)
	}
	wg.Wait()
	sort.SliceStable(seen, func(i, j int) bool { return less(seen[i].view, seen[j].view) })

	if !withTail {
		views := make([]paneView, len(seen))
		for i, s := range seen {
			views[i] = s.view
		}
		return emit(o, map[string]any{"panes": views})
	}
	briefs := make([]briefView, len(seen))
	for i, s := range seen {
		briefs[i] = toBrief(s.view, s.raw, textual.CleanTail(s.raw, lines), s.entries)
	}
	return emit(o, map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"panes":        briefs,
	})
}

// sighting — всё, что собрано про одну панель за этот запуск. Держится вместе,
// потому что порядок вывода задаётся видом, а печатаются и хвост, и история.
type sighting struct {
	view    paneView
	raw     string
	entries []textual.HistoryEntry
}

// Порядок вывода: сначала деятельные панели, внутри — по свежести истории,
// панели без истории в конец своей группы. Смотрящий читает список сверху и
// должен первым делом видеть то, что происходит сейчас.
var activeFirst = map[string]int{"working": 0, "done": 1, "idle": 2}

func less(a, b paneView) bool {
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

// fillHistory возвращает тексты записей: по ним, а не по живому экрану,
// собираются сигналы — в истории видно, над чем панель работает, даже когда
// экран занят выводом команды.
func fillHistory(v *paneView, db *store.Store, p state.Pane, limit, chars int, doneAt time.Time) []textual.HistoryEntry {
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
		last := time.Unix(d.LastTS, 0)
		ts := last.Format(time.RFC3339)
		v.TranscriptID, v.EntryCount, v.LastActivity = &id, &n, &ts
		v.Stale = freshness.Check(last,
			freshness.LastRecord(d.SourcePath, last), doneAt, time.Now())
		out := make([]textual.HistoryEntry, 0, len(d.Entries))
		for _, e := range d.Entries {
			out = append(out, textual.HistoryEntry{Kind: e.Kind, Text: e.Text})
		}
		return out
	}
	return nil
}

// journalFacts — один проход по журналу даёт два ответа: какие панели сейчас
// под наблюдением и когда на каждой последний раз завершалось поручение.
// Второе служит свидетелем свежести истории: журнал достоверен о завершении,
// а история может отставать.
type journalFacts struct {
	watched  map[string]bool
	lastDone map[string]time.Time
	records  []journal.Record
}

func readJournal() journalFacts {
	f := journalFacts{watched: map[string]bool{}, lastDone: map[string]time.Time{}}
	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return f
	}
	f.records = recs
	open := map[string]string{}
	for _, r := range recs {
		switch r.Event {
		case journal.Started:
			open[r.Task] = r.Pane
		case journal.Finished:
			delete(open, r.Task)
			if r.Pane != "" && r.Time.After(f.lastDone[r.Pane]) {
				f.lastDone[r.Pane] = r.Time
			}
		}
	}
	for _, pane := range open {
		f.watched[pane] = true
	}
	return f
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

type digestView struct {
	Target  string       `json:"target"`
	Alias   *string      `json:"alias"`
	Live    *liveView    `json:"live"`
	History historyView  `json:"history"`
	Tail    []string     `json:"tail"`
	Signals briefSignals `json:"signals"`
}

type liveView struct {
	Label      string         `json:"label"`
	Kind       string         `json:"kind"`
	PaneID     string         `json:"pane_id"`
	SessionID  *string        `json:"session_id"`
	Status     string         `json:"status"`
	ContextPct *int           `json:"context_pct"`
	Limits     map[string]int `json:"limits"`
	Title      *string        `json:"title"`
	CWD        string         `json:"cwd"`
	Focused    bool           `json:"focused"`
}

type historyView struct {
	TranscriptID *int64 `json:"transcript_id"`
	// provider берётся из индекса, а не зашит: у панели Codex он не claude.
	Provider     string      `json:"provider,omitempty"`
	EntryCount   *int        `json:"entry_count"`
	LastActivity *string     `json:"last_activity,omitempty"`
	Entries      []entryLine `json:"entries,omitempty"`
	// Stale появляется, только когда история доказуемо отстаёт. Её отсутствие
	// значит «сверено и свежо», а не «не проверяли».
	Stale  *freshness.Lag `json:"stale,omitempty"`
	Reason string         `json:"reason,omitempty"`
}

type entryLine struct {
	ID        int64  `json:"id"`
	TS        string `json:"ts"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Chars     int    `json:"chars"`
	Truncated bool   `json:"truncated"`
}

// cmdDigest — единственная команда, не попавшая в первую сверку, и она
// единственная разошлась с договором: печатала текст, тогда как README и
// SKILL.md описывают JSON из четырёх частей. По этим полям чужой агент решает,
// можно ли давать панели новую задачу.
func cmdDigest(o opts, q string) error {
	db, err := store.Open(o.db)
	if err != nil {
		return err
	}
	defer db.Close()

	p, err := resolve(q)
	if err != nil {
		// Живой панели нет, но цель могла прийти из find: там ключом служит
		// закрытая сессия или идентификатор разговора без неё.
		hist, entries, hErr := digestHistory(db, q, o, time.Time{})
		if hErr != nil {
			return err // живой панели нет и в индексе пусто — цель не найдена
		}
		return emit(o, digestView{
			Target: q, History: hist, Signals: signalsOf("", entries),
		})
	}

	c := client()
	agents, err := c.Agents()
	if err != nil {
		return exitcode.Wrap(exitcode.NoHerdr, err)
	}
	labels := labelsByPane(c, agents)
	raw := rawTail(c, p, o.tailLines)
	target := firstNonEmpty(labels[p.ID], firstNonEmpty(p.Name, p.ID))
	key := p.SessionID
	if key == "" {
		key = q
	}
	// Журнал — достоверный источник завершения: если поручение на этой панели
	// закончилось позже последней записи истории, история отстаёт.
	hist, entries, _ := digestHistory(db, key, o, readJournal().lastDone[p.ID])
	if p.SessionID == "" {
		hist.Reason = "у панели нет session_id"
	}

	g := textual.Gauges(raw)
	v := digestView{
		Target: target,
		Live: &liveView{
			Label: labels[p.ID], Kind: p.Kind, PaneID: p.ID, Status: p.Status,
			ContextPct: g.ContextPct, Limits: g.Limits,
			Title: orNull(p.Title), CWD: p.CWD, Focused: p.Focused,
		},
		History: hist,
		Tail:    textual.CleanTail(raw, o.tailLines),
		Signals: signalsOf(raw, entries),
	}
	if p.Name != "" {
		name := p.Name
		v.Alias = &name
	}
	if p.SessionID != "" {
		sid := p.SessionID
		v.Live.SessionID = &sid
	}
	if len(v.Tail) == 0 {
		v.Tail = nil
	}
	return emit(o, v)
}

func digestHistory(db *store.Store, key string, o opts, doneAt time.Time) (historyView, []textual.HistoryEntry, error) {
	var h historyView
	d, err := db.Digest(key, o.limit, o.chars)
	if err != nil {
		h.Reason = "сессии нет в индексе — возможно, он не пересобирался"
		return h, nil, err
	}
	id, n := d.ConvID, d.EntryCount
	last := time.Unix(d.LastTS, 0)
	ts := last.Format(time.RFC3339)
	h.TranscriptID, h.EntryCount, h.LastActivity = &id, &n, &ts
	h.Provider = d.Agent
	h.Stale = freshness.Check(last,
		freshness.LastRecord(d.SourcePath, last), doneAt, time.Now())

	entries := make([]textual.HistoryEntry, 0, len(d.Entries))
	h.Entries = make([]entryLine, 0, len(d.Entries))
	for _, e := range d.Entries {
		text, cut := textual.Cut(e.Text, o.chars)
		h.Entries = append(h.Entries, entryLine{
			ID: e.ID, TS: time.Unix(e.TS, 0).Format(time.RFC3339),
			Role: e.Kind, Text: text, Chars: e.Len, Truncated: cut,
		})
		entries = append(entries, textual.HistoryEntry{Kind: e.Kind, Text: e.Text})
	}
	return h, entries, nil
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
		return exitcode.Errorf(exitcode.BadCall, "нужен запрос")
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
		return exitcode.Errorf(exitcode.BadCall, "нужна цель")
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

	if err := wakeIsPossible(j, o); err != nil {
		return err
	}

	if o.noWait {
		a, err := client().Get(p.ID)
		if err != nil {
			return exitcode.Wrap(exitcode.NoHerdr, err)
		}
		// Та же проверка, что и у ожидающей ветки: blocked значит, что панель
		// ждёт решения человека, и текст уедет ответом на этот вопрос.
		if !task.Free(a.Status) && !o.force {
			return exitcode.Errorf(exitcode.Busy, "панель занята: %s в состоянии %q", p.ID, a.Status)
		}
		id := task.NewID()
		wake := task.ResolveWake(client(), o.notify, o.notifyThread)
		j.Append(journal.Record{Task: id, Event: journal.Started, Pane: p.ID,
			Target: wake.Target, TargetSession: wake.Session, TargetKind: wake.Kind,
			Prompt: prompt})
		if _, err := client().Prompt(p.ID, prompt, nil, 0); err != nil {
			return exitcode.Wrap(exitcode.BadCall, err)
		}
		// Отправка без ожидания тоже попадает в журнал: иначе `tasks` о ней
		// умолчит, а панель не получит watched в sessions.
		j.Append(journal.Record{Task: id, Event: journal.Finished, Pane: p.ID,
			Outcome: "отправлено без ожидания"})
		return emit(o, map[string]any{"task": id, "pane": p.ID, "sent": true, "waited": false})
	}

	if o.detach {
		// Отсоединённый наблюдатель пишет в свой журнал; без пробуждения его
		// результат не прочтёт никто.
		// Умолчание — та самая сессия, которая поручение затевает: будить
		// кого-то ещё можно только назвав его прямо.
		//
		// Тред Codex здесь предпочтительнее панели: очередь треда не зависит
		// от связи «родитель-потомок», которую --detach как раз и рвёт.
		if o.notify == "" && o.notifyThread == "" {
			if t := codex.ThreadID(); t != "" {
				o.notifyThread = t
			} else {
				o.notify = os.Getenv("HERDR_PANE_ID")
			}
		}
		if o.notify == "" && o.notifyThread == "" {
			return exitcode.Errorf(exitcode.BadCall,
				"--detach без --notify, без --notify-thread и вне панели herdr: результат некому прочитать")
		}
		return detach(o, p.ID, prompt)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := task.Delegate(ctx, task.Options{
		Client: client(), Journal: j, Pane: p.ID, Prompt: prompt,
		Timeout: o.timeout, Force: o.force,
		Notify: o.notify, NotifyThread: o.notifyThread,
	})
	if err != nil {
		if errors.Is(err, task.ErrBusy) {
			return exitcode.Wrap(exitcode.Busy, err)
		}
		return exitcode.Wrap(exitcode.BadCall, err)
	}

	out := map[string]any{
		// Привязка видна сразу: без неё пробуждение не состоится, и узнать об
		// этом лучше здесь, а не через полчаса.
		"wake": map[string]any{"target": res.WakeTarget, "kind": res.WakeKind,
			"session_bound": res.WakeSession != ""},
		"task": res.Task, "pane": p.ID, "outcome": res.Outcome,
		"said": res.Said, "reason": res.Reason,
		"correlated": res.Outcome == task.Reported,
		"seconds":    int(res.Duration.Seconds()),
	}
	// Пробуждение — только по прямой просьбе. Адрес, снятый из окружения,
	// записывается для позднего отчёта, но сам ход не будит: вызывающий и так
	// ждёт этот процесс своим харнессом, и вторая доставка была бы дублем.
	if o.notify != "" || o.notifyThread != "" {
		text := fmt.Sprintf("Поручение %s на панели %s: %s. %s %s",
			res.Task, p.ID, res.Outcome, res.Said, res.Reason)
		out["notified"] = task.Deliver(ctx, client(), j, res.Task, res.WakeTarget, text,
			task.DeliverOptions{Stage: task.StageFinished, Kind: res.WakeKind,
				WantSession: res.WakeSession, Deadline: o.notifyWait})
	}
	if err := emit(o, out); err != nil {
		return err
	}
	switch res.Outcome {
	case task.TimedOut:
		return exitcode.Errorf(exitcode.Timeout, "задача %s не уложилась в срок", res.Task)
	case task.Unknown:
		return exitcode.Errorf(exitcode.Unknown,
			"задача %s: herdr отказал во время ожидания, исход неизвестен", res.Task)
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
	// Потомок должен получить всё, что меняет его поведение: иначе он откажет
	// в свой лог, которого никто не читает.
	args := []string{"delegate", pane, prompt, "--timeout", o.timeoutRaw, "--db-path", o.db}
	if o.notify != "" {
		args = append(args, "--notify", o.notify, "--notify-timeout", o.notifyWaitRaw)
	}
	// Тред передаётся прямо, а не через окружение: потомок живёт своей группой
	// процессов и переживает вызывающего, а CODEX_THREAD_ID у него к тому
	// времени может уже ничего не значить.
	if o.notifyThread != "" {
		args = append(args, "--notify-thread", o.notifyThread, "--notify-timeout", o.notifyWaitRaw)
	}
	if o.force {
		args = append(args, "--force")
	}
	// Каталог заводится здесь же: журнал поручений и индекс создают его сами,
	// а до первого из них --detach падал на «no such file or directory».
	if err := os.MkdirAll(filepath.Dir(detachLog()), 0o755); err != nil {
		return exitcode.Wrap(exitcode.Fail, err)
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
	// Release на unix обнуляет Pid, поэтому номер снимается до него, а не после.
	pid := cmd.Process.Pid
	cmd.Process.Release()
	return emit(o, map[string]any{
		"pane": pane, "detached": true, "pid": pid, "log": detachLog(),
	})
}

// sessionOfPane — разговор, живущий сейчас в панели.
func sessionOfPane(pane string) string {
	if pane == "" {
		return ""
	}
	a, err := client().Get(pane)
	if err != nil {
		return ""
	}
	return a.Session.Value
}

func detachLog() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudex", "detached.log")
}

// wakeIsPossible отказывает до отправки, если пробуждение некуда адресовать.
// Отказ уходит в ход вызывающего: он успевает сделать иначе, а не узнаёт о
// потере из лога, который никто не читает.
func wakeIsPossible(j *journal.Journal, o opts) error {
	if o.notify == "" && o.notifyThread == "" && !o.detach {
		return nil
	}
	wake := task.ResolveWake(client(), o.notify, o.notifyThread)

	if wake.Kind == task.KindThread {
		// У треда доказывать нечего, кроме того, что он открыт: адрес и есть
		// разговор, промахнуться соседним нельзя.
		if v := task.MayQueue(wake.Target, codex.State(codex.Home(), wake.Target)); !v.OK() {
			return exitcode.Errorf(exitcode.BadCall,
				"результат будет некуда доставить: %s.", v.Reason())
		}
		return nil
	}

	if v := task.MayWrite(wake.Target, wake.Session, wake.Session,
		task.SharedPane(j, wake.Target)); !v.OK() {
		return exitcode.Errorf(exitcode.BadCall,
			"результат будет некуда доставить: %s.\n"+
				"Назовите тред прямо: --notify-thread <id> кладёт отчёт в очередь разговора, "+
				"а не в панель, за которой их несколько.\n"+
				"Либо запустите без --detach и --notify, а ждите своим харнессом: в Codex это "+
				"exec(…, yield_time_ms) и wait(cell_id) — дескриптор держит только этот "+
				"разговор, и промахнуться нечем. Потерян дескриптор — claudex tasks --task <id>.",
			v.Reason())
	}
	return nil
}

// taskWindow — границы поручения и панель, где оно выполнялось, по журналу.
func taskWindow(id string) (prompt string, from, to time.Time, pane string) {
	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return
	}
	for _, r := range recs {
		if r.Task != id {
			continue
		}
		switch r.Event {
		case journal.Started:
			if from.IsZero() {
				from = r.Time
			}
			if r.Prompt != "" {
				prompt = r.Prompt
			}
			if r.Pane != "" {
				pane = r.Pane
			}
		case journal.Finished, journal.Reported:
			to = r.Time
		}
	}
	return
}

// taskDigest собирает ход работы за окно поручения. Источников может не быть
// ни одного — дайджест тогда состоит из названных причин, и это правильнее
// молчания.
func taskDigest(o opts, id, prompt string, from, to time.Time, pane string) digest.Digest {
	d := digest.Options{Task: id, Prompt: prompt, From: from, To: to, Pane: pane,
		Chars: o.chars, TailLines: o.tailLines}
	if pane != "" {
		d.Key = sessionOfPane(pane)
		c := client()
		d.Tail = func(target string, lines int) ([]string, error) {
			p, err := resolve(target)
			if err != nil {
				return nil, err
			}
			return tailFor(c, p, lines), nil
		}
	}
	db, err := store.Open(o.db)
	if err == nil {
		defer db.Close()
		d.LookupHead = func(key string) (digest.Head, error) {
			h, err := db.Digest(key, 1, 1)
			if err != nil {
				return digest.Head{}, err
			}
			return digest.Head{LastActivity: time.Unix(h.LastTS, 0),
				SourcePath: h.SourcePath, Entries: h.EntryCount}, nil
		}
		d.Entries = db.Since
	}
	return digest.Build(d)
}

func cmdTaskDigest(o opts, id string) error {
	prompt, from, to, pane := taskWindow(id)
	if from.IsZero() {
		return exitcode.Errorf(exitcode.NotFound, "поручения %s в журнале нет", id)
	}
	d := taskDigest(o, id, prompt, from, to, pane)
	if o.pretty {
		return emit(o, d)
	}
	fmt.Print(d.Text("(дайджест по запросу)"))
	return nil
}

func cmdDone(o opts, id, reason string) error {
	outcome := "готово"
	if i := strings.IndexByte(reason, ' '); i > 0 && isOutcomeWord(reason[:i]) {
		outcome, reason = reason[:i], strings.TrimSpace(reason[i+1:])
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := task.Report(ctx, id, outcome, reason, task.ReportOptions{
		Client: client(), Journal: journal.Open(defaultJournal()),
		// Ведущему уходит не только строка исхода: по одной строке продолжать
		// планирование нельзя, а перечитывать транскрипт руками он не обязан.
		Compose: func(summary string, from, to time.Time, pane string) string {
			prompt, _, _, _ := taskWindow(id)
			return taskDigest(o, id, prompt, from, to, pane).Text(summary)
		},
	})
	if err != nil {
		return err
	}
	return emit(opts{}, res)
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

func cmdTasks(o opts, args []string) error {
	recs, err := journal.Open(defaultJournal()).Read()
	if err != nil {
		return err
	}
	// Один идентификатор — это адресация без панелей: его держит только тот,
	// кто поручение затеял.
	if id := firstNonEmpty(o.task, first(args)); id != "" {
		st := task.StateOfTask(recs, id)
		if !st.Known {
			return exitcode.Errorf(exitcode.NotFound, "поручения %s в журнале нет", id)
		}
		return emit(o, st)
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Time.Before(recs[j].Time) })
	for _, r := range recs {
		where := r.Pane
		if r.Event == journal.Notified {
			where = r.Target
			if r.Stage != "" {
				where += "/" + r.Stage
			}
		}
		fmt.Printf("%s %-8s %-9s %-8s %s %s\n",
			r.Time.Format("02.01 15:04"), r.Task, r.Event, where, r.Outcome, oneLine(r.Reason))
	}
	if undelivered := undeliveredWakes(recs); len(undelivered) > 0 {
		fmt.Printf("\nне доставлено ведущему: %s\n", strings.Join(undelivered, ", "))
	}
	return nil
}

// undeliveredWakes — поручения, которые просили разбудить ведущего, но он так
// и не был разбужен. Без этой строки сбой виден только в логе отсоединённого
// наблюдателя, куда никто не смотрит.
func undeliveredWakes(recs []journal.Record) []string {
	type key struct{ task, stage string }
	seen, woken, order := map[key]string{}, map[key]bool{}, []key{}
	for _, r := range recs {
		if r.Event != journal.Notified {
			continue
		}
		k := key{r.Task, r.Stage}
		if _, ok := seen[k]; !ok {
			order = append(order, k)
		}
		seen[k] = r.Outcome
		if r.Outcome == task.WokeUp {
			woken[k] = true
		}
	}
	var out []string
	for _, k := range order {
		if woken[k] {
			continue
		}
		stage := k.stage
		if stage == "" {
			stage = task.StageFinished
		}
		out = append(out, fmt.Sprintf("%s/%s (%s)", k.task, stage, seen[k]))
	}
	return out
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func oneID(args []string) (int64, error) {
	if len(args) == 0 {
		return 0, exitcode.Errorf(exitcode.BadCall, "нужен номер записи")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, exitcode.Errorf(exitcode.BadCall, "номер записи — число, получено %q", args[0])
	}
	return id, nil
}

func oneLine(s string) string { return cut(strings.Join(strings.Fields(s), " "), 160) }

func cut(s string, max int) string {
	out, _ := textual.Cut(strings.Join(strings.Fields(s), " "), max)
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
