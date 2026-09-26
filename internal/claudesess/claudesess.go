// Пакет claudesess адресует живые разговоры Claude Code на этой машине.
//
// У каждой сессии есть входящий сокет, а рядом с ним — запись в реестре
// `~/.claude/sessions/<pid>.json` с разговором, именем, каталогом и
// занятостью. Отсюда вторая адресация в claudex: панель herdr переживает
// смену агента, и «панель свободна» разговора не доказывает, а сокет и есть
// разговор.
//
// Панель этим не заменяется. Сокет доставляет текст в уже существующий
// разговор и не умеет ни запустить сессию, ни прочитать её экран.
package claudesess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrNoTarget = errors.New("адресат не назван")
	ErrNotFound = errors.New("среди живых сессий такой нет")
	ErrSelf     = errors.New("это текущая сессия")
)

// Ambiguous — под названное подходит несколько разговоров.
//
// Отдельным типом, а не текстом: вызывающему нужен список, чтобы человек
// выбрал сам. Подставить «похожую» значит увести поручение в чужой разговор.
type Ambiguous struct {
	Target     string
	Candidates []Session
}

func (a *Ambiguous) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "под %q подходит %s, назовите идентификатор:",
		a.Target, plural(len(a.Candidates)))
	full := !distinctShorts(a.Candidates)
	for _, s := range a.Candidates {
		id := s.Short()
		if full {
			// Кандидаты совпали короткими видами — а по ним и предлагается
			// выбрать. Список из неразличимых строк выбора не даёт, поэтому
			// показывается идентификатор целиком, готовый к вставке.
			id = s.ID
		}
		fmt.Fprintf(&b, "\n  %s  %s  %s", id, s.Name, s.CWD)
	}
	return b.String()
}

func distinctShorts(in []Session) bool {
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if seen[s.Short()] {
			return false
		}
		seen[s.Short()] = true
	}
	return true
}

func plural(n int) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return fmt.Sprintf("%d разговор", n)
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return fmt.Sprintf("%d разговора", n)
	default:
		return fmt.Sprintf("%d разговоров", n)
	}
}

// Session — живой разговор Claude Code.
type Session struct {
	PID      int       `json:"pid"`
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	CWD      string    `json:"cwd,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Status   string    `json:"status,omitempty"`
	Socket   string    `json:"socket,omitempty"`
	StatusAt time.Time `json:"status_at,omitempty"`
}

// Busy — занят ли разговор прямо сейчас. Реестр знает два состояния, и всё,
// что не «busy», занятостью не считается: неизвестное состояние — не повод
// утверждать, что работа идёт.
func (s Session) Busy() bool { return s.Status == "busy" }

// Short — короткий вид разговора, каким его показывает сам Claude Code.
func (s Session) Short() string {
	if len(s.ID) >= 8 {
		return s.ID[:8]
	}
	return s.ID
}

// Home — каталог реестра сессий.
func Home() string {
	if p := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); p != "" {
		return filepath.Join(p, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions")
}

// запись реестра в том виде, в каком её пишет Claude Code.
type record struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Name            string `json:"name"`
	CWD             string `json:"cwd"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	Socket          string `json:"messagingSocketPath"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
}

// List — живые разговоры, по возрастанию pid.
//
// Живой значит, что сокет на месте. Записи переживают свои процессы: в живом
// реестре их было 36 при 18 живых сокетах, и выдавать осиротевшую запись за
// адрес значит обещать доставку туда, где никто не слушает.
func List(home string) []Session {
	if home == "" {
		return nil
	}
	names, err := filepath.Glob(filepath.Join(home, "*.json"))
	if err != nil {
		return nil
	}
	var out []Session
	for _, p := range names {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r record
		if err := json.Unmarshal(b, &r); err != nil || r.Socket == "" {
			continue
		}
		if fi, err := os.Stat(r.Socket); err != nil || fi.Mode()&os.ModeSocket == 0 {
			continue
		}
		s := Session{
			PID: r.PID, ID: r.SessionID, Name: r.Name, CWD: r.CWD,
			Kind: r.Kind, Status: r.Status, Socket: r.Socket,
		}
		if r.StatusUpdatedAt > 0 {
			s.StatusAt = time.UnixMilli(r.StatusUpdatedAt)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Resolve выбирает разговор по убыванию точности: полный идентификатор,
// его однозначное начало, затем имя.
//
// Имена повторяются — в живом реестре три сессии звались «license», — поэтому
// неоднозначность заканчивается отказом со списком, а не выбором наугад.
func Resolve(home, target string) (Session, error) { return lookup(home, target, true) }

// Lookup — то же, но без отбрасывания текущей сессии.
//
// Отбрасывание нужно там, где адрес называет человек: Claude Code откажет в
// сообщении самому себе, и доводить до этого отказа незачем. При доставке
// адрес приходит из журнала, а не от человека, и запись в собственный ящик
// сессии Claude Code поддерживает прямо — так доставляются сообщения от
// собственных потомков. Дожим зависших отчётов выполняется при любом вызове
// claudex, в том числе изнутри самого адресата: отказ по «это текущая сессия»
// оставил бы такой отчёт висеть навсегда.
func Lookup(home, target string) (Session, error) { return lookup(home, target, false) }

func lookup(home, target string, dropSelf bool) (Session, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return Session{}, ErrNoTarget
	}
	live := List(home)

	for _, match := range []func(Session) bool{
		func(s Session) bool { return s.ID == target },
		func(s Session) bool { return len(target) >= 8 && strings.HasPrefix(s.ID, target) },
		func(s Session) bool { return s.Name == target },
		func(s Session) bool { return strings.EqualFold(s.Name, target) },
	} {
		var hit []Session
		for _, s := range live {
			if match(s) {
				hit = append(hit, s)
			}
		}
		if len(hit) == 0 {
			continue
		}
		rest := hit
		if dropSelf {
			rest = withoutSelf(hit)
		}
		if len(rest) == 0 {
			return Session{}, ErrSelf
		} else if len(rest) == 1 {
			return rest[0], nil
		} else {
			return Session{}, &Ambiguous{Target: target, Candidates: rest}
		}
	}
	return Session{}, ErrNotFound
}

// withoutSelf убирает из кандидатов текущую сессию: Claude Code отказывает в
// сообщении самому себе, и доводить до этого отказа незачем.
func withoutSelf(in []Session) []Session {
	self := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	if self == "" {
		return in
	}
	var out []Session
	for _, s := range in {
		if s.ID != self {
			out = append(out, s)
		}
	}
	return out
}

// SendTimeout — сколько держать соединение. Claude Code закрывает соединение,
// не приславшее целой строки за полминуты; нам столько не нужно, текст готов
// до того, как мы постучались.
const SendTimeout = 10 * time.Second

// Send кладёт текст в разговор.
//
// Кадр — тот, что показывает сам Claude Code в своём образце, и он проверен на
// живой сессии: сообщение дошло. Ключ шлётся только в собственный ящик: им
// Claude Code опознаёт своего потомка, когда доказательств по процессу уже
// нет, а чужому адресату он бессмыслен.
func Send(ctx context.Context, s Session, text string) error {
	if s.Socket == "" {
		return fmt.Errorf("%w: адреса нет", ErrNotFound)
	}
	d := net.Dialer{Timeout: SendTimeout}
	conn, err := d.DialContext(ctx, "unix", s.Socket)
	if err != nil {
		return fmt.Errorf("разговор %s не принял соединение: %w", s.Short(), err)
	}
	defer conn.Close()
	if t, ok := ctx.Deadline(); ok {
		conn.SetWriteDeadline(t)
	} else {
		conn.SetWriteDeadline(time.Now().Add(SendTimeout))
	}

	var b strings.Builder
	if tok := ownToken(s.Socket); tok != "" {
		line, _ := json.Marshal(map[string]string{"type": "auth", "token": tok})
		b.Write(line)
		b.WriteByte('\n')
	}
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]string{"role": "user", "content": text},
	})
	if err != nil {
		return err
	}
	b.Write(line)
	b.WriteByte('\n')

	if _, err := conn.Write([]byte(b.String())); err != nil {
		return fmt.Errorf("разговор %s не принял сообщение: %w", s.Short(), err)
	}
	return nil
}

// ownToken — ключ, если адресат и есть наш собственный ящик.
func ownToken(socket string) string {
	if socket == "" || socket != strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")) {
		return ""
	}
	return strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"))
}

// ErrNoPickup — разговор так и не взялся за поручение.
//
// Отдельной причиной, а не таймаутом: «простаивал с самого начала» и
// «поработал и закончил» выглядят в реестре одинаково, а значат
// противоположное. Считать первое завершением значит выдать несделанное за
// сделанное — ровно та ошибка, из-за которой «панель свободна» перестали
// считать доказательством.
var ErrNoPickup = errors.New("разговор не взялся за поручение")

// WaitIdle ждёт, пока разговор возьмётся за работу и снова освободится.
//
// Завершением считается только пара переходов: сначала занят, потом свободен.
// pickup — сколько ждать первого из них.
func WaitIdle(ctx context.Context, home, id string, poll, pickup time.Duration) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if pickup <= 0 {
		pickup = 30 * time.Second
	}
	deadline := time.Now().Add(pickup)
	took := false
	for {
		s, err := Resolve(home, id)
		if err != nil {
			// Разговор закрыли на полпути. Молчаливый успех тут хуже ошибки:
			// поручение считалось бы выполненным.
			return fmt.Errorf("разговор %s пропал: %w", shortOf(id), err)
		}
		switch {
		case s.Busy():
			took = true
		case took:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("%w за %s", ErrNoPickup, pickup)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func shortOf(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
