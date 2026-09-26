// Package digest собирает, что на самом деле происходило внутри сессии Claude
// за время поручения.
//
// Финальная строка отчёта — это вывод, а не работа. Ведущему для продолжения
// планирования нужно видеть ход: что поручили, какие команды шли и чем
// кончились, что поменялось, что проверено и где уперлось. Брать для этого
// транскрипт целиком нельзя по двум причинам сразу: он огромен и он лежит в
// индексе cass, который отстаёт — измерено, на часы.
//
// Отсюда три правила, которые и составляют смысл пакета:
//
// Окно поручения закрыто с обеих сторон. До начала работы в том же разговоре
// шла другая, и выдать её за свою — худший вид неверного отчёта.
//
// Индекс, не дотянувшийся до начала окна, не даёт ничего. Показать вместо
// работы более старые записи — значит соврать уверенно; лучше пустой раздел и
// названная причина.
//
// Живой хвост панели берётся всегда и помечается отдельно: он свежий по
// построению, потому что читается из herdr, а не из индекса.
package digest

import (
	"fmt"
	"strings"
	"time"

	"github.com/surraulistic/claudex/internal/freshness"
	"github.com/surraulistic/claudex/internal/store"
	"github.com/surraulistic/claudex/internal/textual"
)

// Пределы для сообщения, которое уезжает в разговор ведущего.
//
// Толкать полный ход дорого и чаще всего зря: детали нужны примерно одной
// задаче из пяти, а платит за них каждая. Замерено — дайджест в callback
// занимал около 1240 токенов на задачу при 74 доставках. Поэтому в тред уходит
// скелет, а полный ход берётся по запросу командой `claudex digest`.
const (
	PushEntries   = 14
	PushChars     = 160
	PushToolChars = 90
	PushBudget    = 1200
)

const (
	DefaultEntries   = 60
	DefaultChars     = 600
	DefaultToolChars = 220
	DefaultTailLines = 16
	DefaultBudget    = 12000
)

// Head — что индекс знает о разговоре: время последней записи и путь к
// транскрипту. Пустой путь значит «сверить свежесть не с чем».
type Head struct {
	LastActivity time.Time
	SourcePath   string
	Entries      int
}

// Options — источники и границы. Функции подменяются в тестах: настоящие ходят
// в SQLite и в сокет herdr, и проверять ими правила окна незачем.
type Options struct {
	Task    string
	Prompt  string
	Outcome string
	Reason  string

	// Key — чем разговор адресуется в индексе (session_id либо id разговора).
	Key  string
	Pane string

	// From — начало поручения, To — его конец. Нулевой To читается как «ещё
	// идёт»: окно закрывается текущим временем.
	From time.Time
	To   time.Time

	LookupHead func(key string) (Head, error)
	Entries    func(key string, fromMS, toMS int64, limit, chars int) ([]store.Entry, error)
	Tail       func(pane string, lines int) ([]string, error)

	MaxEntries int
	Chars      int
	ToolChars  int
	TailLines  int
	Budget     int
	Now        time.Time
}

type Line struct {
	TS   time.Time `json:"ts"`
	Role string    `json:"role"`
	Text string    `json:"text"`
	Tool bool      `json:"tool"`
}

type Digest struct {
	Task    string `json:"task"`
	Prompt  string `json:"prompt,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`

	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	// Talk — реплики, Work — записи инструментов: команды, их вывод, правки.
	Talk []Line   `json:"talk,omitempty"`
	Work []Line   `json:"work,omitempty"`
	Tail []string `json:"tail,omitempty"`

	// Stale — доказанное отставание индекса, если оно есть.
	Stale *freshness.Lag `json:"stale,omitempty"`
	// Covered — покрывает ли индекс окно поручения. Ложь значит, что разделы
	// Talk и Work пусты не потому, что работы не было.
	Covered bool `json:"covered"`
	// Dropped — сколько записей окна не поместилось в бюджет.
	Dropped int      `json:"dropped,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

func (o *Options) fill() {
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultEntries
	}
	if o.Chars <= 0 {
		o.Chars = DefaultChars
	}
	if o.ToolChars <= 0 {
		o.ToolChars = DefaultToolChars
	}
	if o.TailLines <= 0 {
		o.TailLines = DefaultTailLines
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.To.IsZero() {
		o.To = o.Now
	}
}

// Build никогда не возвращает ошибку: недостающий источник — это заметка в
// дайджесте, а не отказ. Отчёт должен уйти даже тогда, когда индекса нет вовсе.
func Build(o Options) Digest {
	o.fill()
	d := Digest{
		Task: o.Task, Prompt: o.Prompt, Outcome: o.Outcome, Reason: o.Reason,
		From: o.From, To: o.To,
	}

	d.Tail = liveTail(o)
	fillHistory(&d, o)
	return d
}

func liveTail(o Options) []string {
	if o.Tail == nil || o.Pane == "" {
		return nil
	}
	lines, err := o.Tail(o.Pane, o.TailLines)
	if err != nil || len(lines) == 0 {
		return nil
	}
	return lines
}

func fillHistory(d *Digest, o Options) {
	if o.LookupHead == nil || o.Entries == nil || o.Key == "" {
		d.note("история не читалась: разговор не назван или индекс недоступен")
		return
	}
	head, err := o.LookupHead(o.Key)
	if err != nil {
		d.note("разговора нет в индексе — вероятно, он ещё не пересобирался: " + err.Error())
		return
	}

	d.Stale = freshness.Check(head.LastActivity,
		freshness.LastRecord(head.SourcePath, head.LastActivity), o.To, o.Now)

	// Индекс, чья последняя запись старше начала окна, про эту работу не знает
	// ничего. Разделы остаются пустыми намеренно.
	if !head.LastActivity.IsZero() && head.LastActivity.Before(o.From) {
		d.note(fmt.Sprintf(
			"индекс не покрывает окно поручения: последняя запись %s, поручение начато %s — показывать нечего, кроме живого хвоста",
			head.LastActivity.In(time.Local).Format("15:04:05"),
			o.From.In(time.Local).Format("15:04:05")))
		return
	}

	rows, err := o.Entries(o.Key, o.From.UnixMilli(), o.To.UnixMilli(),
		o.MaxEntries+1, o.Chars)
	if err != nil {
		d.note("история окна не прочиталась: " + err.Error())
		return
	}
	d.Covered = true
	if len(rows) > o.MaxEntries {
		d.Dropped += len(rows) - o.MaxEntries
		rows = rows[:o.MaxEntries]
	}
	if len(rows) == 0 {
		d.note("в окне поручения индекс записей не содержит")
		return
	}

	spent := 0
	for _, e := range rows {
		limit := o.Chars
		if e.Tool {
			limit = o.ToolChars
		}
		text, _ := textual.Cut(e.Text, limit)
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if spent+len(text) > o.Budget {
			d.Dropped++
			continue
		}
		spent += len(text)
		l := Line{TS: time.Unix(e.TS, 0), Role: e.Kind, Text: text, Tool: e.Tool}
		if e.Tool {
			d.Work = append(d.Work, l)
		} else {
			d.Talk = append(d.Talk, l)
		}
	}
	if d.Dropped > 0 {
		d.note(fmt.Sprintf("в бюджет не поместилось записей: %d — полный ход смотреть через claudex", d.Dropped))
	}
}

func (d *Digest) note(s string) { d.Notes = append(d.Notes, s) }

// Text — вид, в котором дайджест уезжает в очередь треда.
//
// Первой строкой сказано, чем является сообщение: ведущий не должен принимать
// сводку за прочитанный им самим ход работы.
func (d Digest) Text(summary string) string {
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n— — —\nЭто сводка от ClauDex, а не ответ Claude: Claude здесь доносчик.\n")
	b.WriteString(fmt.Sprintf("Поручение %s · окно %s — %s\n",
		d.Task,
		d.From.In(time.Local).Format("15:04:05"),
		d.To.In(time.Local).Format("15:04:05")))

	if d.Prompt != "" {
		b.WriteString("\n## Что поручили\n")
		p, _ := textual.Cut(d.Prompt, 700)
		b.WriteString(p + "\n")
	}

	section(&b, "## Ход работы (команды и результаты)", d.Work)
	section(&b, "## Что говорилось", d.Talk)

	if len(d.Tail) > 0 {
		b.WriteString("\n## Живой хвост панели (из herdr, свежий)\n")
		for _, l := range d.Tail {
			b.WriteString("  " + l + "\n")
		}
	}

	if d.Stale != nil {
		b.WriteString("\n## Свежесть истории\n  " + d.Stale.Reason + "\n")
	}
	if !d.Covered {
		b.WriteString("\n## Внимание\n  Индекс окно поручения не покрыл: разделы хода пусты не потому, что работы не было.\n")
	}
	for _, n := range d.Notes {
		b.WriteString("  · " + n + "\n")
	}

	b.WriteString("\n## Прежде чем решать\n")
	b.WriteString("  Это сжатая выжимка. За полным ходом — `claudex digest " + d.Task + "`,\n")
	b.WriteString("  за состоянием репозитория — `git -C <репо> log`, `git status`, `git show`.\n")
	return b.String()
}

// PushOptions — пределы для сообщения в тред. Накладываются поверх того, что
// задал вызывающий.
func PushOptions(o Options) Options {
	o.MaxEntries, o.Chars = PushEntries, PushChars
	o.ToolChars, o.Budget = PushToolChars, PushBudget
	o.TailLines = 8 // пригодится, если индекс окно не покрыл
	return o
}

// Push — то, что уезжает в разговор ведущего.
//
// От полного вида отличается тремя решениями. Промпт не возвращается: его
// написал сам ведущий, и он у него в контексте. Реплики не возвращаются: их
// пересказывает сводка. Живой хвост панели не возвращается: это картинка
// терминала, полезная человеку, а не разговору.
//
// Остаётся то, чего у ведущего нет: что делалось, что помешало, и чем добрать.
func (d Digest) Push(summary string) string {
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n— — —\nСводка ClauDex, не ответ Claude.\n")

	if len(d.Work) > 0 {
		b.WriteString("\nДелалось:\n")
		for _, l := range d.Work {
			b.WriteString("  · " + oneLine(l.Text) + "\n")
		}
	}
	if d.Dropped > 0 {
		b.WriteString(fmt.Sprintf("  (ещё %d шагов не показано)\n", d.Dropped))
	}
	if !d.Covered {
		// Индекс до окна не дотянулся — значит живой экран панели остаётся
		// единственным доказательством работы, и выбрасывать его тут нельзя.
		// Иначе ведущий получает «показывать нечего» и ничего больше.
		//
		// Но заголовок ставится только к непустому хвосту: у поручения в
		// разговор Claude Code панели нет вовсе, и обещание показать её экран
		// выполнено не будет. Пустой раздел с таким заголовком — это ссылка на
		// доказательство, которого не существует.
		if tail := tailFew(d.Tail); len(tail) > 0 {
			b.WriteString("\nИндекс окно не покрыл, с живого экрана панели:\n")
			for _, l := range tail {
				b.WriteString("  " + l + "\n")
			}
		} else {
			b.WriteString("\nИндекс окно не покрыл, показать нечего.\n")
		}
	} else if d.Stale != nil {
		b.WriteString("\n" + d.Stale.Reason + "\n")
	}
	b.WriteString("\nПодробнее: `claudex digest " + d.Task + "` · репозиторий проверять глазами.\n")
	return b.String()
}

// tailFew — сколько строк хвоста уезжает в тред как доказательство. Берём
// конец: там финал, а не приглашение.
func tailFew(tail []string) []string {
	const n = 6
	if len(tail) > n {
		return tail[len(tail)-n:]
	}
	return tail
}

func section(b *strings.Builder, title string, lines []Line) {
	if len(lines) == 0 {
		return
	}
	b.WriteString("\n" + title + "\n")
	for _, l := range lines {
		b.WriteString(fmt.Sprintf("  [%s %s] %s\n",
			l.TS.In(time.Local).Format("15:04:05"), l.Role, oneLine(l.Text)))
	}
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " ⏎ "))
}
