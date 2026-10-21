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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// Follow отмечает завершение ходов, начатых в тредах Codex. Настоящий —
	// task.FollowTurns: только читает app-server.
	Follow func(context.Context, *journal.Journal, int) []task.FollowedTurn
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
	TurnsDone   int       `json:"turns_done"`
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
	// TurnsDone — ходы в тредах Codex, чьё завершение замечено за проход.
	TurnsDone []task.FollowedTurn `json:"turns_done,omitempty"`
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
	if o.Follow == nil {
		o.Follow = task.FollowTurns
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
	silent := map[string]bool{}
	t.Silent = s.wakeSilent(ctx, recs, now, silent)

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
		} else if silent[l.Task] {
			// Пробуждение за молчуна повторяет wakeSilent со своим событием и
			// своей отсрочкой; второй попытки за тот же тик не нужно.
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
	t.TurnsDone = s.o.Follow(ctx, s.o.Journal, s.o.MaxPerTick)
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

// wakeSilent будит затеявшего за тех, кто закончил ход и замолчал, и отмечает
// в owned всех молчунов: их повторы ведёт он, а не дожим.
//
// Событие говорит ровно то, что видно: работа кончилась, вот последняя
// реплика. Исхода оно не называет — исход даёт только сама задача.
//
// Разбуженным считается только то, что дошло: событие, принятое в очередь
// треда без начатого хода, откладывается, как любой отказ. Предел MaxPerTick
// — на попытки, а не на удачи.
func (s *Supervisor) wakeSilent(ctx context.Context, recs []journal.Record, now time.Time,
	owned map[string]bool) []string {
	ctx, cancel := context.WithTimeout(ctx, s.o.Budget)
	defer cancel()
	var out []string
	tried := 0
	for _, sl := range task.SilentTasks(recs, now, s.o.SilenceGrace) {
		owned[sl.Task] = true
		if tried >= s.o.MaxPerTick || ctx.Err() != nil {
			continue
		}
		if until, ok := s.next["тишина:"+sl.Task]; ok && now.Before(until) {
			continue
		}
		st := task.StateOf(recs, sl.Task)
		session := st.CallerSession
		if session == "" && st.CallerKind != task.KindPane {
			session = st.Caller
		}
		w := task.WakeOf(st.CallerKind, st.Caller, session)
		if w.Target == "" {
			continue
		}
		tried++
		text := ""
		if !task.WakeCapable(w.Kind) {
			// Панель событие не читает: ей нужен текст, иначе уйдёт пустая реплика.
			text = task.SilenceReport(sl, st.Executor)
		}
		d := task.Deliver(ctx, s.o.Client, s.o.Journal, sl.Task, w.Target,
			text, task.DeliverOptions{
				Stage: task.StageSilence, Kind: w.Kind, WantSession: w.Session,
				HumanTold: true, Event: task.SilenceEvent(sl, st.Executor),
				Deadline: silenceDeadline(w.Kind), Poll: time.Second,
			})
		if d.Reached() {
			out = append(out, sl.Task)
			delete(s.next, "тишина:"+sl.Task)
			delete(s.wait, "тишина:"+sl.Task)
			continue
		}
		s.defer_("тишина:"+sl.Task, now)
	}
	return out
}

// silenceDeadline — сколько ждать одно пробуждение: панель ждём недолго, а
// пробуждение треда — цепочка вызовов app-server.
func silenceDeadline(kind string) time.Duration {
	if kind == task.KindThread {
		return 10 * time.Second
	}
	return 5 * time.Second
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
	s.stat.TurnsDone += len(t.TurnsDone)
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
	s.writeStatus()
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
// Живость — только замок рядом с файлом. Файл переживает убитый процесс, а
// его pid после перезагрузки достаётся кому угодно: живой чужой процесс
// выдавал мёртвого наблюдателя за работающего, и тот больше не поднимался.
// Файл остаётся статистикой; если замок держит не тот, кто его писал, она
// чужая и не возвращается.
func Read(path string) (Status, bool) {
	var st Status
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &st)
	}
	pid, held := Holder(lockNear(path))
	if !held {
		return st, false
	}
	if pid > 0 && pid != st.PID {
		return Status{PID: pid}, true
	}
	return st, true
}

func lockNear(statusPath string) string {
	if statusPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statusPath), "locks", "supervisor.lock")
}

// ErrRunning — замок наблюдателя уже держит другой процесс.
var ErrRunning = errors.New("наблюдатель уже работает")

// LockPath — замок единственного наблюдателя. Файл состояния для этого не
// годится: два процесса, запущенных разом, оба видят его пустым.
func LockPath() string { return lockNear(StatusPath()) }

// Acquire берёт замок на всё время жизни процесса; ОС снимает его и при
// аварийном выходе.
func Acquire(path string) (release func(), err error) {
	if path == "" {
		return nil, errors.New("путь замка наблюдателя неизвестен")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrRunning
		}
		return nil, err
	}
	if f.Truncate(0) == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Holder — держит ли кто-нибудь замок и какой pid он в нём записал; 0 —
// не записал.
func Holder(path string) (int, bool) {
	if !Held(path) {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, true
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, true
}

// Held — держит ли кто-нибудь замок. Надёжнее проверки по PID из файла
// состояния: номер процесса переиспользуется. Pid в замке не переписывает.
func Held(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
