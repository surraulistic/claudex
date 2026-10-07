// Package headless starts tracked Claude Code background sessions.
package headless

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const startTimeout = 30 * time.Second

type Session struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	CWD       string `json:"cwd"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	StartedAt int64  `json:"startedAt"`
}

func ShortID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Start launches one native Claude Code background session. It deliberately
// does not select a permission mode: the session inherits the user's and
// project's normal Claude Code permissions.
func Start(ctx context.Context, cwd, taskID, _sessionID, prompt string) (Session, error) {
	if strings.TrimSpace(cwd) == "" {
		return Session{}, fmt.Errorf("рабочий каталог headless-сессии не задан")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return Session{}, fmt.Errorf("рабочий каталог %q: %w", cwd, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Session{}, fmt.Errorf("рабочий каталог %q недоступен: %w", abs, err)
	}
	if !info.IsDir() {
		return Session{}, fmt.Errorf("рабочий каталог %q — не папка", abs)
	}

	bin, err := exec.LookPath("claude")
	if err != nil {
		return Session{}, fmt.Errorf("Claude Code не найден в PATH: %w", err)
	}
	name := "claudex-" + taskID
	args := []string{"--bg", "--name", name, prompt}
	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	cmd := exec.CommandContext(startCtx, bin, args...)
	cmd.Dir = abs
	cmd.Env = withTaskEnv(os.Environ(), taskID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len([]rune(msg)) > 1200 {
			msg = string([]rune(msg)[:1200]) + "…"
		}
		if msg != "" {
			return Session{}, fmt.Errorf("Claude Code не запустил фоновую сессию: %w: %s", err, msg)
		}
		return Session{}, fmt.Errorf("Claude Code не запустил фоновую сессию: %w", err)
	}
	return latestSession(ctx, bin, name, abs, "")
}

// Resume continues a tracked background session with another instruction.
func Resume(ctx context.Context, cwd, taskID, sessionID, prompt string) (Session, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return Session{}, fmt.Errorf("Claude Code не найден в PATH: %w", err)
	}
	args := []string{"--resume", sessionID, "--bg", "--name", "claudex-" + taskID, prompt}
	resumeCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	cmd := exec.CommandContext(resumeCtx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = withTaskEnv(os.Environ(), taskID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len([]rune(msg)) > 1200 {
			msg = string([]rune(msg)[:1200]) + "…"
		}
		if msg != "" {
			return Session{}, fmt.Errorf("Claude Code не продолжил headless-сессию: %w: %s", err, msg)
		}
		return Session{}, fmt.Errorf("Claude Code не продолжил headless-сессию: %w", err)
	}
	return latestSession(ctx, bin, "claudex-"+taskID, cwd, sessionID)
}

func latestSession(ctx context.Context, bin, name, cwd, fallbackID string) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var lastErr error
	for {
		cmd := exec.CommandContext(ctx, bin, "agents", "--json", "--all")
		out, err := cmd.Output()
		if err == nil {
			var sessions []Session
			if err = json.Unmarshal(out, &sessions); err == nil {
				var matches []Session
				for _, s := range sessions {
					if s.Kind == "background" && s.SessionID != "" &&
						(s.Name == name || strings.HasPrefix(s.Name, name+" (")) {
						matches = append(matches, s)
					}
				}
				if len(matches) > 0 {
					sort.SliceStable(matches, func(i, j int) bool {
						return matches[i].StartedAt > matches[j].StartedAt
					})
					return matches[0], nil
				}
				lastErr = fmt.Errorf("фоновый разговор %s ещё не появился в claude agents --json", name)
			} else {
				lastErr = fmt.Errorf("прочитать claude agents --json: %w", err)
			}
		} else {
			lastErr = fmt.Errorf("claude agents --json: %w", err)
		}
		select {
		case <-ctx.Done():
			if fallbackID != "" {
				return Session{ID: ShortID(fallbackID), SessionID: fallbackID,
					Name: name, CWD: cwd, Kind: "background", State: "working"}, nil
			}
			return Session{}, lastErr
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func withTaskEnv(env []string, taskID string) []string {
	const key = "CLAUDEX_TASK_ID="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key) {
			out = append(out, e)
		}
	}
	return append(out, key+taskID)
}
