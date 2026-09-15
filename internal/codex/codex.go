// Package codex адресует отчёт разговору Codex, а не панели.
//
// Панель адресом быть не может: измерено, что Codex ведёт несколько разговоров
// разом за одной панелью, а herdr называет из них только один — отчёт уходил в
// разговор, который о задаче не знал. У `codex queue` адрес — сам тред, и
// промахнуться нечем.
//
// Идентификатор своего треда процесс узнаёт из CODEX_THREAD_ID: его кладёт в
// окружение сам Codex, и на него же опирается встроенная интеграция herdr
// (~/.codex/herdr-agent-state.sh). Выводить его из чего-либо ещё —
// из времени файла снимка оболочки, из предка процесса — нельзя: обе догадки
// промахиваются ровно там, где разговоров несколько, то есть там, где это и
// нужно.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Состояние треда — ровно то, что можно доказать, читая $CODEX_HOME.
const (
	// Live — у треда есть свежий замок писателя: он открыт и примет очередь.
	Live = "live"
	// Known — тред известен указателю сессий, но замка нет: закрыт или
	// заархивирован. Сообщение в такой уедет в никуда.
	Known = "known"
	// Unknown — про этот идентификатор здесь не знает никто.
	Unknown = "unknown"
)

// ThreadID — идентификатор разговора Codex, который запустил этот процесс.
// Пусто значит «нас запустил не Codex» либо «Codex его не сообщил»: и то и
// другое — «подтвердить нечем», а не «наверное, тот же».
//
// Переменная наследуется потомками, а Codex умеет запускать Claude Code. Тогда
// она у него есть, но означает не «мой ведущий», а «дед по процессу»: отчёт
// уехал бы в тред, который поручения не давал — ровно та ошибка, ради которой
// адресация по разговору и заводилась. Claude Code отличим по собственным
// переменным, и при них унаследованный идентификатор адресом не считается.
func ThreadID() string {
	if os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_SESSION_ID") != "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
}

// Home — каталог состояния Codex.
func Home() string {
	if p := strings.TrimSpace(os.Getenv("CODEX_HOME")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// State — что про тред известно на диске. Отдельно от доставки: отказ должен
// быть объясним, а не сводиться к ненулевому коду возврата чужой команды.
func State(home, id string) string {
	if home == "" || !IsThreadID(id) {
		return Unknown
	}
	if _, err := os.Stat(filepath.Join(home, "thread-writer-locks", id+".lock")); err == nil {
		return Live
	}
	if indexKnows(filepath.Join(home, "session_index.jsonl"), id) {
		return Known
	}
	return Unknown
}

// IsThreadID — похоже ли это на идентификатор разговора.
//
// `codex queue --thread` принимает и имя сессии, но имя адресом быть не может:
// в указателе сессий их три подряд зовутся «license service», и доставка по
// имени — это ровно тот промах мимо разговора, ради которого всё затевалось.
// Заодно отсекается `.coordination`: каталог замков держит не только треды.
func IsThreadID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// indexKnows — встречается ли идентификатор в указателе сессий. Указатель
// дописывается, поэтому один тред лежит в нём многократно; достаточно первого
// совпадения.
func indexKnows(path, id string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.ID == id {
			return true
		}
	}
	return false
}

// Queue ставит сообщение в очередь названного треда.
//
// Подменяется в тестах: настоящий вызов будит живой разговор, и проверять им
// правило отказа нельзя.
var Queue = func(ctx context.Context, id, message string) error {
	cmd := exec.CommandContext(ctx, binary(), "queue", "--thread", id, "--message", message)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return fmt.Errorf("codex queue: %w: %s", err, oneLine(s))
	}
	return fmt.Errorf("codex queue: %w", err)
}

func binary() string {
	if p := strings.TrimSpace(os.Getenv("CLAUDEX_CODEX_BIN")); p != "" {
		return p
	}
	return "codex"
}

// Send — доставка с повторами. Отказ бывает преходящим: демон app-server
// перезапускается, и следующая попытка через пару секунд проходит. Долго
// держать нельзя — эта ветка выполняется в том числе внутри хода самой задачи.
type SendOptions struct {
	Attempts int
	Gap      time.Duration
	Each     time.Duration
}

func Send(ctx context.Context, id, message string, o SendOptions) error {
	if o.Attempts <= 0 {
		o.Attempts = 3
	}
	if o.Gap <= 0 {
		o.Gap = 2 * time.Second
	}
	if o.Each <= 0 {
		o.Each = 15 * time.Second
	}
	var last error
	for i := 0; i < o.Attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w (после %d попыток: %v)", ctx.Err(), i, last)
			case <-time.After(o.Gap):
			}
		}
		attempt, cancel := context.WithTimeout(ctx, o.Each)
		err := Queue(attempt, id, message)
		cancel()
		if err == nil {
			return nil
		}
		last = err
	}
	return last
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " · ")
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}
