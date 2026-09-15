package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const thread = "01a0a51e-20c4-7e10-8488-b524b9389489"

// home собирает поддельный $CODEX_HOME: замки писателей и указатель сессий —
// ровно то, по чему состояние треда доказывается на живой машине.
func home(t *testing.T, live []string, known []string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "thread-writer-locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range live {
		p := filepath.Join(dir, "thread-writer-locks", id+".lock")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var index []byte
	for _, id := range append(append([]string{}, live...), known...) {
		index = append(index, []byte(`{"id":"`+id+`","thread_name":"t","updated_at":"2026-09-15T12:50:10Z"}`+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, "session_index.jsonl"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStateSeparatesLiveFromClosedFromUnknown(t *testing.T) {
	// Три состояния, и их нельзя сводить к двум: закрытый тред примет очередь
	// молча и не прочитает её никогда, поэтому «известен» — это не «жив».
	dir := home(t, []string{thread}, []string{"01a0a508-1a8d-7ff1-b2b3-47b5e6ceb96b"})
	for _, c := range []struct{ name, id, want string }{
		{"замок на месте", thread, Live},
		{"замка нет, указатель помнит", "01a0a508-1a8d-7ff1-b2b3-47b5e6ceb96b", Known},
		{"не знает никто", "01a0ffff-0000-7000-8000-000000000000", Unknown},
		{"пустой идентификатор", "", Unknown},
	} {
		if got := State(dir, c.id); got != c.want {
			t.Errorf("%s: получено %q, ожидалось %q", c.name, got, c.want)
		}
	}
}

func TestOnlyAnIdentifierIsAnAddress(t *testing.T) {
	// Имя сессии адресом быть не может: в указателе три треда подряд зовутся
	// «license service», и доставка по имени попала бы в любой из них.
	// Заодно отсекается .coordination — каталог замков держит не только треды.
	for _, c := range []struct {
		id   string
		want bool
	}{
		{thread, true},
		{"01A0A51E-20C4-7E10-8488-B524B9389489", true},
		{"license service", false},
		{".coordination", false},
		{"", false},
		{"01a0a51e20c47e108488b524b9389489", false},
		{"01a0a51e-20c4-7e10-8488-b524b938948z", false},
		{"01a0a51e-20c4-7e10-8488-b524b9389489-", false},
	} {
		if got := IsThreadID(c.id); got != c.want {
			t.Errorf("IsThreadID(%q) = %v, ожидалось %v", c.id, got, c.want)
		}
	}
}

func TestStrayLockDoesNotMakeAThread(t *testing.T) {
	// Живой замок .coordination.lock лежит рядом с тредовыми, и раньше зонд
	// докладывал про него «положим в очередь».
	dir := home(t, []string{thread}, nil)
	if err := os.WriteFile(filepath.Join(dir, "thread-writer-locks", ".coordination.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := State(dir, ".coordination"); got != Unknown {
		t.Fatalf("получено %q, ожидалось %q", got, Unknown)
	}
}

func TestStateOfMissingHomeIsUnknownNotLive(t *testing.T) {
	// Нет каталога — нет доказательства. Разрешать доставку «по умолчанию»
	// здесь значит стрелять в темноту.
	if got := State(filepath.Join(t.TempDir(), "нет-такого"), thread); got != Unknown {
		t.Fatalf("получено %q, ожидалось %q", got, Unknown)
	}
}

func TestThreadIDComesOnlyFromTheEnvironment(t *testing.T) {
	// Выводить идентификатор из времени файлов или предка процесса нельзя:
	// обе догадки промахиваются ровно там, где разговоров несколько.
	// Прогон тестов запущен из-под Claude Code, а его переменные гасят
	// унаследованный идентификатор — здесь проверяется не это.
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "  "+thread+"  ")
	if got := ThreadID(); got != thread {
		t.Fatalf("получено %q, ожидалось %q", got, thread)
	}
	t.Setenv("CODEX_THREAD_ID", "")
	if got := ThreadID(); got != "" {
		t.Fatalf("без переменной идентификатора нет, получено %q", got)
	}
}

func TestInheritedThreadIDIsNotAnAddress(t *testing.T) {
	// Codex умеет запускать Claude Code, и тот наследует CODEX_THREAD_ID.
	// Означает он тогда «дед по процессу», а не «мой ведущий»: отчёт уехал бы
	// в тред, который поручения не давал.
	for _, v := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CODEX_THREAD_ID", thread)
			t.Setenv("CLAUDECODE", "")
			t.Setenv("CLAUDE_CODE_SESSION_ID", "")
			t.Setenv(v, "1")
			if got := ThreadID(); got != "" {
				t.Fatalf("унаследованный идентификатор адресом не считается, получено %q", got)
			}
		})
	}
}

func TestSendRetriesTransientFailure(t *testing.T) {
	// Демон app-server перезапускается, и первая попытка падает; вторая
	// проходит. Считать первый отказ окончательным — терять отчёт на ровном
	// месте.
	calls := 0
	restore := swap(func(context.Context, string, string) error {
		calls++
		if calls < 2 {
			return errors.New("connection refused")
		}
		return nil
	})
	defer restore()

	if err := Send(context.Background(), thread, "готово",
		SendOptions{Gap: time.Millisecond}); err != nil {
		t.Fatalf("вторая попытка прошла, получено %v", err)
	}
	if calls != 2 {
		t.Fatalf("попыток %d, ожидалось 2", calls)
	}
}

func TestSendGivesUpAndKeepsTheLastReason(t *testing.T) {
	restore := swap(func(context.Context, string, string) error {
		return errors.New("тред закрыт")
	})
	defer restore()

	err := Send(context.Background(), thread, "готово",
		SendOptions{Attempts: 2, Gap: time.Millisecond})
	if err == nil {
		t.Fatal("три отказа — это отказ, а не успех")
	}
	if err.Error() != "тред закрыт" {
		t.Fatalf("причина последней попытки сохранена, получено %q", err)
	}
}

func TestSendStopsOnContextCancel(t *testing.T) {
	restore := swap(func(context.Context, string, string) error {
		return errors.New("отказ")
	})
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Send(ctx, thread, "готово", SendOptions{Gap: time.Second}); err == nil {
		t.Fatal("отменённый контекст — не успех")
	}
}

func swap(f func(context.Context, string, string) error) func() {
	prev := Queue
	Queue = f
	return func() { Queue = prev }
}
