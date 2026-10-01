// Пакет supervise доводит отчёты до того, кто поручение затеял, без участия
// человека.
//
// Зачем. Отчёт бывает написан и не доставлен: разговор затеявшего успел
// закрыться. Текст при этом цел и лежит в канале вытягивания, но уходит он
// только при следующем вызове claudex — то есть когда кто-то и так что-то
// делает. Если не делает, отчёт ждёт неделями, а для затеявшего это
// неотличимо от молчания. Замерено на живом журнале: 132 таких отчёта.
//
// Чего наблюдатель не делает. Он не сочиняет отчёт и вообще ничего не пишет от
// себя: пересказ работы, сделанный не тем, кто её делал, — это выдумка с
// интонацией уверенности. Он замечает смену состояния и будит затеявшего, а
// тот сам читает `claudex task` и `claudex task digest`.
//
// Почему не LLM. Здесь нечего решать: доставить готовый текст по записанному
// адресу — работа на сравнение строк. Языковая модель добавила бы стоимость,
// задержку и возможность соврать.
package supervise

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/surraulistic/claudex/internal/herdr"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

const (
	// DefaultInterval — как часто заглядывать. Редко намеренно: ждать тут
	// нечего, а разговор, закрывшийся на час, за минуту не откроется.
	DefaultInterval = 30 * time.Second
	// DefaultMaxPerTick — сколько отчётов дожимать за раз. Предел держит тик
	// коротким и не даёт наблюдателю разом разбудить десяток разговоров.
	DefaultMaxPerTick = 3
	// DefaultBudget — предел на один тик целиком.
	DefaultBudget = 20 * time.Second
	// BackoffBase и BackoffMax — насколько отложить адрес, который только что
	// отказал. Без этого наблюдатель долбится в закрытый разговор каждый тик.
	BackoffBase = 2 * time.Minute
	BackoffMax  = 30 * time.Minute
)

type Options struct {
	Journal *journal.Journal
	Client  *herdr.Client
	// StatusPath — куда писать состояние, чтобы doctor мог его прочесть.
	StatusPath string
	Interval   time.Duration
	MaxPerTick int
	Budget     time.Duration
	// Reconcile включает сборку отчётов за молчунов. По умолчанию выключена:
	// сборка читает живые экраны и трогает куда больше поручений, чем дожим,
	// поэтому сперва она должна пожить под присмотром.
	Reconcile bool
	// SilenceGrace — сколько ждать после окончания хода, прежде чем считать
	// молчание окончательным.
	SilenceGrace time.Duration
	// ArchiveAfter — с какой давности уносить поручения в архив. Ноль значит
	// «не уносить»: журнал — единственная память об этой работе, и решать за
	// человека, когда её перекладывать, инструмент не должен. Включается
	// флагом, и тогда наблюдатель делает это раз в сутки.
	ArchiveAfter time.Duration
	// JournalPath нужен архивации: она работает с файлом, а не с записями.
	JournalPath string

	Now func() time.Time
	// Flush подменяется в тестах. Настоящий — task.Flush.
	Flush func(context.Context, task.FlushOptions) []task.Flushed
}

// Status — то, что наблюдатель оставляет о себе на диске.
type Status struct {
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"started_at"`
	LastTick    time.Time `json:"last_tick"`
	Ticks       int       `json:"ticks"`
	Delivered   int       `json:"delivered"`
	Failed      int       `json:"failed"`
	Pending     int       `json:"pending"`
	Deferred    int       `json:"deferred"`
	Woken       int       `json:"woken"`
	Archived    int       `json:"archived"`
	ArchiveNote string    `json:"archive_note,omitempty"`
	Interval    string    `json:"interval"`
	Reconciling bool      `json:"reconciling"`
}

// Tick — итог одного прохода.
type Tick struct {
	Delivered []string `json:"delivered,omitempty"`
	Failed    []string `json:"failed,omitempty"`
	Pending   int      `json:"pending"`
	Deferred  int      `json:"deferred"`
	// Silent — за кого разбудили по молчанию, без отчёта от самой задачи.
	Silent []string `json:"silent,omitempty"`
}

// Supervisor хранит то, чего нет в журнале: когда следующий раз трогать адрес,
// который отказал.
type Supervisor struct {
	o           Options
	next        map[string]time.Time
	wait        map[string]time.Duration
	stat        Status
	lastArchive time.Time
}

func New(o Options) *Supervisor {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.MaxPerTick <= 0 {
		o.MaxPerTick = DefaultMaxPerTick
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Flush == nil {
		o.Flush = task.Flush
	}
	return &Supervisor{
		o: o, next: map[string]time.Time{}, wait: map[string]time.Duration{},
		stat: Status{PID: os.Getpid(), StartedAt: o.Now(),
			Interval: o.Interval.String(), Reconciling: o.Reconcile},
	}
}

// Once делает один проход.
func (s *Supervisor) Once(ctx context.Context) Tick {
	now := s.o.Now()
	var t Tick

	recs, err := s.o.Journal.Read()
	if err != nil {
		return t
	}
	// Молчуны проверяются первыми и независимо от зависших отчётов: если
	// исполнитель доработал и не отчитался, затеявший не узнает вообще ничего,
	// а недоставленный отчёт хотя бы существует. Прежде эта проверка стояла
	// после раннего выхода «дожимать нечего» и при пустом списке не
	// выполнялась вовсе — поймано тестом.
	t.Silent = s.wakeSilent(ctx, recs, now)

	// Считаем то, что дожимаем. Брошенное видно в `claudex undelivered`, а
	// здесь оно создавало бы впечатление очереди, которая не двигается.
	lost := task.LivePending(recs, now)
	t.Pending = len(lost)

	// Отложенные считаются и пропускаются: адрес отказал недавно, и повторять
	// попытку сейчас незачем.
	skip := map[string]bool{}
	for _, l := range lost {
		if until, ok := s.next[l.Task]; ok && now.Before(until) {
			skip[l.Task] = true
			t.Deferred++
		}
	}
	// Ранний выход отсюда убран намеренно. Дважды он отрезал то, что стояло
	// ниже: сперва пробуждение за молчунов, потом уборку журнала. Выход
	// посреди функции — не невнимательность, а форма, которая к этому
	// располагает; поэтому дожим отделён, а обязательное идёт после него
	// безусловно.
	if t.Deferred < len(lost) {
		s.flushPending(ctx, &t, skip, now)
	}
	s.archiveOnce(now)
	s.record(now, t)
	return t
}

// flushPending досылает то, что не дошло. Всё, что «всё отложено» — обычный
// исход, а не сбой.
func (s *Supervisor) flushPending(ctx context.Context, t *Tick, skip map[string]bool, now time.Time) {
	for _, f := range s.o.Flush(ctx, task.FlushOptions{
		Journal: s.o.Journal, Client: s.o.Client,
		Max: s.o.MaxPerTick, Budget: s.o.Budget, Skip: skip,
	}) {
		if f.Ok {
			t.Delivered = append(t.Delivered, f.Task)
			delete(s.next, f.Task)
			delete(s.wait, f.Task)
			continue
		}
		t.Failed = append(t.Failed, f.Task)
		s.defer_(f.Task, now)
	}
}

// archiveOnce уносит давнее в архив не чаще раза в сутки.
//
// Реже, чем тик, намеренно: журнал переписывается целиком, и делать это каждую
// минуту — тратить работу впустую и держать окно, в котором сбой застанет
// перезапись.
func (s *Supervisor) archiveOnce(now time.Time) {
	if s.o.ArchiveAfter <= 0 || s.o.JournalPath == "" {
		return
	}
	if !s.lastArchive.IsZero() && now.Sub(s.lastArchive) < 24*time.Hour {
		return
	}
	s.lastArchive = now
	res, err := journal.Archive(s.o.JournalPath, s.o.ArchiveAfter, false)
	if err != nil {
		s.stat.ArchiveNote = "архивация не прошла: " + err.Error()
		return
	}
	s.stat.Archived += res.Moved
	if res.Moved > 0 {
		s.stat.ArchiveNote = fmt.Sprintf("перенесено %d записей по %d поручениям в %s",
			res.Moved, res.Tasks, filepath.Base(res.Path))
	}
}

// wakeSilent будит затеявшего за тех, кто закончил ход и замолчал.
//
// Событие говорит ровно то, что видно: работа кончилась, вот последняя
// реплика. Исхода оно не называет — исход даёт только сама задача.
func (s *Supervisor) wakeSilent(ctx context.Context, recs []journal.Record, now time.Time) []string {
	var out []string
	for _, sl := range task.SilentTasks(recs, now, s.o.SilenceGrace) {
		if len(out) >= s.o.MaxPerTick || ctx.Err() != nil {
			break
		}
		if until, ok := s.next["тишина:"+sl.Task]; ok && now.Before(until) {
			continue
		}
		st := task.StateOf(recs, sl.Task)
		w := task.WakeOf(st.CallerKind, st.Caller, st.Caller)
		if w.Target == "" {
			continue
		}
		d := task.Deliver(ctx, s.o.Client, s.o.Journal, sl.Task, w.Target,
			"", task.DeliverOptions{
				Stage: task.StageReported, Kind: w.Kind, WantSession: w.Session,
				HumanTold: true, Event: task.SilenceEvent(sl, st.Executor),
				Deadline: 5 * time.Second, Poll: time.Second,
			})
		if d.OK {
			out = append(out, sl.Task)
			continue
		}
		s.defer_("тишина:"+sl.Task, now)
	}
	return out
}

// defer_ откладывает адрес вдвое дальше прошлого раза, но не дальше предела.
func (s *Supervisor) defer_(id string, now time.Time) {
	w := s.wait[id]
	if w == 0 {
		w = BackoffBase
	} else {
		w *= 2
	}
	if w > BackoffMax {
		w = BackoffMax
	}
	s.wait[id] = w
	s.next[id] = now.Add(w)
}

func (s *Supervisor) record(now time.Time, t Tick) {
	s.stat.LastTick = now
	s.stat.Ticks++
	s.stat.Delivered += len(t.Delivered)
	s.stat.Failed += len(t.Failed)
	s.stat.Pending, s.stat.Deferred = t.Pending, t.Deferred
	s.stat.Woken += len(t.Silent)
	s.writeStatus()
}

func (s *Supervisor) writeStatus() {
	if s.o.StatusPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.o.StatusPath), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(s.stat, "", "  ")
	if err != nil {
		return
	}
	// Запись через временный файл: doctor читает этот же путь, и застать его
	// наполовину записанным он не должен.
	tmp := s.o.StatusPath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, s.o.StatusPath)
	}
}

// Run крутит наблюдение до отмены.
func (s *Supervisor) Run(ctx context.Context) error {
	s.Once(ctx)
	t := time.NewTicker(s.o.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.clearStatus()
			return ctx.Err()
		case <-t.C:
			s.Once(ctx)
		}
	}
}

// clearStatus убирает файл состояния: остановленный наблюдатель не должен
// выглядеть работающим.
func (s *Supervisor) clearStatus() {
	if s.o.StatusPath != "" {
		os.Remove(s.o.StatusPath)
	}
}

func (s *Supervisor) Status() Status { return s.stat }

// StatusPath — где наблюдатель оставляет о себе запись.
func StatusPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claudex", "supervisor.json")
}

// Read — состояние наблюдателя и работает ли он.
//
// Живость проверяется по процессу, а не по файлу: файл переживает убитый
// процесс, и doctor, доверившись ему, сказал бы «всё под присмотром» там, где
// присмотра нет.
func Read(path string) (Status, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Status{}, false
	}
	var st Status
	if json.Unmarshal(b, &st) != nil || st.PID == 0 {
		return Status{}, false
	}
	p, err := os.FindProcess(st.PID)
	if err != nil {
		return st, false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return st, false
	}
	return st, true
}
