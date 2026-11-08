package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Кто держит замок писателя треда. ChatGPT Desktop держит свои треды
// app-server'ом на stdio: сокета у него нет, демон видит их notLoaded, и
// достучаться до него можно только через общую очередь, которую каждый
// app-server опрашивает сам (ext/queue, watch_external_messages, 0.167.0).
const (
	// OwnerNone — замка нет: тред не загружен нигде.
	OwnerNone = ""
	// OwnerAppServer — замок держит другой app-server Codex: он читает общую
	// очередь сам.
	OwnerAppServer = "app_server"
	// OwnerForeign — замок держит процесс, который не app-server Codex:
	// очередь он не читает, а второй писатель недопустим.
	OwnerForeign = "foreign"
	// OwnerUnknown — замок есть, а кто его держит, установить не удалось.
	OwnerUnknown = "unknown"
)

type Owner struct {
	Kind string
	PID  int
	Exe  string
	Args string
	Why  string
}

// Desktop — встроенный runtime приложения, а не отдельно запущенный CLI.
func (o Owner) Desktop() bool {
	return strings.Contains(o.Exe, "/ChatGPT.app/") || strings.Contains(o.Exe, "/Codex.app/")
}

func (o Owner) String() string {
	switch {
	case o.PID == 0:
		return "владелец не установлен"
	case o.Desktop():
		return fmt.Sprintf("pid %d, ChatGPT Desktop", o.PID)
	case o.Exe != "":
		return fmt.Sprintf("pid %d, %s", o.PID, o.Exe)
	}
	return fmt.Sprintf("pid %d", o.PID)
}

// LockHolders — какие процессы держат открытым файл. Сам файл не открывается:
// замок писателя трогать нельзя даже на чтение. Подменяется в тестах.
var LockHolders = func(ctx context.Context, path string) ([]int, error) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-t", "-w", "--", path).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lsof: %w", err)
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("lsof: не pid %q", f)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// ProcessCommand — исполняемый файл и командная строка процесса. Подменяется
// в тестах.
var ProcessCommand = func(ctx context.Context, pid int) (exe, args string, err error) {
	p := strconv.Itoa(pid)
	c, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-o", "comm=", "-p", p).Output()
	if err != nil {
		return "", "", fmt.Errorf("ps comm: %w", err)
	}
	a, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-o", "args=", "-p", p).Output()
	if err != nil {
		return "", "", fmt.Errorf("ps args: %w", err)
	}
	return strings.TrimSpace(string(c)), strings.TrimSpace(string(a)), nil
}

// ThreadOwner — кто держит замок писателя треда. Всё, что не доказано, —
// OwnerUnknown: решать за неизвестного владельца нельзя.
func ThreadOwner(home, id string) Owner {
	if home == "" || !IsThreadID(id) {
		return Owner{Kind: OwnerUnknown, Why: "тред или $CODEX_HOME неизвестны"}
	}
	path := filepath.Join(home, "thread-writer-locks", id+".lock")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return Owner{Kind: OwnerNone}
	} else if err != nil {
		return Owner{Kind: OwnerUnknown, Why: "замок писателя не прочитан: " + err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pids, err := LockHolders(ctx, path)
	if err != nil {
		return Owner{Kind: OwnerUnknown, Why: "держатель замка писателя не найден: " + err.Error()}
	}
	pids = unique(pids)
	switch len(pids) {
	case 0:
		return Owner{Kind: OwnerUnknown, Why: "замок писателя есть, а держащего его процесса не видно"}
	case 1:
	default:
		return Owner{Kind: OwnerUnknown, Why: fmt.Sprintf("замок писателя открыт сразу у %d процессов: %v", len(pids), pids)}
	}
	exe, args, err := ProcessCommand(ctx, pids[0])
	if err != nil {
		return Owner{Kind: OwnerUnknown, PID: pids[0], Why: "командная строка держателя не прочитана: " + err.Error()}
	}
	return classifyOwner(pids[0], exe, args)
}

func unique(pids []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range pids {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// Флаги codex и app-server, за которыми идёт значение.
var valueFlags = map[string]bool{
	"-c": true, "--config": true, "--enable": true, "--disable": true, "-p": true, "--profile": true,
	"--listen": true, "--ws-auth": true, "--ws-token-file": true, "--ws-token-sha256": true,
	"--ws-shared-secret-file": true, "--ws-issuer": true, "--ws-audience": true,
	"--ws-max-clock-skew-seconds": true, "--startup-client-info": true,
	"--cloud-skills-snapshot": true, "--code-mode-host": true,
}

// classifyOwner признаёт хозяином треда только `codex [флаги] app-server
// [флаги]` без подкоманды: `app-server daemon …`, `proxy`, `exec`, TUI и всё
// прочее очередь чужих тредов не обслуживает.
func classifyOwner(pid int, exe, args string) Owner {
	o := Owner{Kind: OwnerForeign, PID: pid, Exe: exe, Args: args}
	if filepath.Base(exe) != "codex" {
		o.Why = "держатель замка — не codex"
		return o
	}
	var rest string
	switch fields := strings.Fields(args); {
	case strings.HasPrefix(args, exe):
		rest = args[len(exe):]
	case len(fields) > 0 && filepath.Base(fields[0]) == "codex":
		rest = strings.TrimPrefix(args, fields[0])
	default:
		o.Kind, o.Why = OwnerUnknown, "командная строка держателя не разобрана"
		return o
	}
	var positional []string
	tokens := strings.Fields(rest)
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if strings.HasPrefix(t, "-") {
			if valueFlags[t] {
				i++
			}
			continue
		}
		positional = append(positional, t)
	}
	switch {
	case len(positional) == 0 || positional[0] != "app-server":
		o.Why = "держатель замка — codex, но не app-server"
	case len(positional) > 1:
		o.Why = "держатель замка — codex app-server " + positional[1] + ", а не сервер тредов"
	default:
		o.Kind = OwnerAppServer
	}
	return o
}
