package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const ownerThread = "01a0cccc-0000-7000-8000-0000000000aa"

func TestClassifyOwner(t *testing.T) {
	daemon := "/Users/u/.codex/packages/standalone/releases/0.167.0-aarch64-apple-darwin/bin/codex"
	desktop := "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
	cases := []struct {
		name, exe, args, kind string
		desktop               bool
	}{
		{"desktop stdio runtime", desktop,
			desktop + " -c features.code_mode_host=true app-server --analytics-default-enabled", OwnerAppServer, true},
		{"remote-control daemon", daemon,
			daemon + " app-server --remote-control --listen unix:// --analytics-default-enabled --managed-daemon",
			OwnerAppServer, false},
		{"argv0 from PATH", daemon, "codex app-server --listen stdio://", OwnerAppServer, false},
		{"updater loop", daemon, daemon + " app-server daemon pid-update-loop", OwnerForeign, false},
		{"proxy", daemon, daemon + " app-server proxy", OwnerForeign, false},
		{"exec resume", daemon, daemon + " exec resume 01a0", OwnerForeign, false},
		{"tui", daemon, daemon + " resume --last", OwnerForeign, false},
		{"not codex", "/usr/bin/python3", "/usr/bin/python3 x.py app-server", OwnerForeign, false},
		{"unparsable", "/Applications/Codex Thing.app/codex", "/elsewhere/bin app-server", OwnerUnknown, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := classifyOwner(77, c.exe, c.args)
			if o.Kind != c.kind || o.PID != 77 || o.Desktop() != c.desktop {
				t.Fatalf("получено %+v desktop=%v", o, o.Desktop())
			}
		})
	}
}

func lockHome(t *testing.T, lock bool) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if lock {
		if err := os.WriteFile(filepath.Join(dir, ownerThread+".lock"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func stubProcs(t *testing.T, holders func(string) ([]int, error), cmd func(int) (string, string, error)) {
	t.Helper()
	ph, pc := LockHolders, ProcessCommand
	LockHolders = func(_ context.Context, p string) ([]int, error) { return holders(p) }
	ProcessCommand = func(_ context.Context, pid int) (string, string, error) { return cmd(pid) }
	t.Cleanup(func() { LockHolders, ProcessCommand = ph, pc })
}

func TestThreadOwner(t *testing.T) {
	desktop := "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
	asDesktop := func(int) (string, string, error) { return desktop, desktop + " app-server", nil }
	cases := []struct {
		name    string
		lock    bool
		holders func(string) ([]int, error)
		cmd     func(int) (string, string, error)
		kind    string
		pid     int
	}{
		{"no lock", false, nil, nil, OwnerNone, 0},
		{"desktop", true, func(string) ([]int, error) { return []int{501, 501}, nil }, asDesktop, OwnerAppServer, 501},
		{"stale lock", true, func(string) ([]int, error) { return nil, nil }, asDesktop, OwnerUnknown, 0},
		{"two holders", true, func(string) ([]int, error) { return []int{1, 2}, nil }, asDesktop, OwnerUnknown, 0},
		{"lsof fails", true, func(string) ([]int, error) { return nil, errors.New("нет lsof") }, asDesktop, OwnerUnknown, 0},
		{"ps fails", true, func(string) ([]int, error) { return []int{9}, nil },
			func(int) (string, string, error) { return "", "", errors.New("процесс ушёл") }, OwnerUnknown, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := lockHome(t, c.lock)
			called := false
			stubProcs(t, func(p string) ([]int, error) {
				called = true
				if want := filepath.Join(home, "thread-writer-locks", ownerThread+".lock"); p != want {
					t.Errorf("смотрит не тот замок: %s", p)
				}
				return c.holders(p)
			}, func(pid int) (string, string, error) { return c.cmd(pid) })
			o := ThreadOwner(home, ownerThread)
			if o.Kind != c.kind || o.PID != c.pid {
				t.Fatalf("получено %+v", o)
			}
			if !c.lock && called {
				t.Fatal("без замка держателя не ищут")
			}
			if o.Kind == OwnerUnknown && o.Why == "" {
				t.Fatal("отказ объясним")
			}
		})
	}
	if o := ThreadOwner("", ownerThread); o.Kind != OwnerUnknown {
		t.Fatalf("без $CODEX_HOME владелец неизвестен: %+v", o)
	}
}
