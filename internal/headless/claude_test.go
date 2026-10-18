package headless

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestShortIDAcceptsFullOrShortSessionID(t *testing.T) {
	if got := ShortID("2b2c3d4e-1111-4111-8111-123456789abc"); got != "2b2c3d4e" {
		t.Fatalf("сокращённый UUID: %q", got)
	}
	if got := ShortID("2b2c3d4e"); got != "2b2c3d4e" {
		t.Fatalf("короткий id сохранён: %q", got)
	}
}

func TestStartUsesNativeBackgroundModeAndNormalPermissions(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(root, "argv.txt")
	fakeClaude := filepath.Join(binDir, "claude")
	script := `#!/bin/sh
if [ "$1" = "agents" ]; then
  printf '[{"id":"d00df00d","sessionId":"d00df00d-1111-4111-8111-111111111111","name":"claudex-abc12345","cwd":"%s","kind":"background","state":"working"}]' "$PROJECT"
  exit 0
fi
printf '%s\n' "$@" > "$CAPTURE"
printf '%s\n' "$CLAUDEX_TASK_ID" >> "$CAPTURE"
printf 'backgrounded · d00df00d · claudex-abc12345\n'
`
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("CAPTURE", capture)
	t.Setenv("PROJECT", projectDir)

	sessionID := "d00df00d-1111-4111-8111-111111111111"
	s, err := Start(context.Background(), projectDir, "abc12345", "",
		"проверь код и верни отчёт")
	if err != nil {
		t.Fatal(err)
	}
	if s.SessionID != sessionID || s.CWD != projectDir || s.Kind != "background" {
		t.Fatalf("неверная привязка процесса: %+v", s)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"--bg", "--name", "claudex-abc12345",
		"проверь код и верни отчёт", "abc12345"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("CLI contract mismatch:\n got %q\nwant %q", got, want)
	}
	for _, arg := range got {
		if strings.Contains(arg, "permission") || strings.Contains(arg, "dangerously") {
			t.Fatalf("не добавлять обход разрешений: %q", arg)
		}
	}
}

func TestOversizedPromptIsRefusedBeforeLaunch(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "launched")
	fake := filepath.Join(root, "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	t.Setenv("PATH", root)
	big := strings.Repeat("я", MaxPromptBytes)
	for _, call := range []func() error{
		func() error { _, err := Start(context.Background(), root, "abc12345", "", big); return err },
		func() error { _, err := Resume(context.Background(), root, "abc12345", "s", big); return err },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "файл") {
			t.Fatalf("отказ с понятной причиной, получено %v", err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("Claude Code не запускался")
	}
}

// stubClaude — `claude --bg` отвечает кодом bgExit, а `claude agents --json`
// сессию не показывает.
func stubClaude(t *testing.T, bgExit int) string {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"agents\" ]; then printf '[]'; exit 0; fi\nexit " +
		strconv.Itoa(bgExit) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return t.TempDir()
}

func TestLaunchedButNotYetListedIsNotALaunchFailure(t *testing.T) {
	// R9: --bg вернул 0, а claude agents сессию за 3 с не показал.
	project := stubClaude(t, 0)
	s, err := Start(context.Background(), project, "abc12345", "", "задача")
	if !errors.Is(err, ErrNotListed) {
		t.Fatalf("запуск удался, привязки нет — это не отказ запуска, получено %v", err)
	}
	if s.Name != "claudex-abc12345" || s.CWD != project || s.SessionID != "" {
		t.Fatalf("известное о запуске сохранено, идентификатор не выдуман: %+v", s)
	}
}

func TestRefusedLaunchIsStillAFailure(t *testing.T) {
	project := stubClaude(t, 3)
	if _, err := Start(context.Background(), project, "abc12345", "", "задача"); err == nil || errors.Is(err, ErrNotListed) {
		t.Fatalf("--bg отказал — отказ запуска, получено %v", err)
	}
}
