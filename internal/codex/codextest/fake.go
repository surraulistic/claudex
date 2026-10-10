// Package codextest — поддельный app-server Codex: настоящий WebSocket поверх
// unix-сокета и JSON-RPC, с поведением очереди, снятым с живого демона 0.162.1.
package codextest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/surraulistic/claudex/internal/codex"
)

type Thread struct {
	Status    codex.ThreadStatus
	Ephemeral bool
	Queue     []codex.QueuedItem
	Turns     []codex.Turn
	// Held — очередь придержана клиентом: сама не вычитывается.
	Held bool
	// NoDrainOnResume — resume грузит тред, но очередь не трогает.
	NoDrainOnResume bool
	// KeepRunning — начатый ход не кончается сам.
	KeepRunning bool
	// FailResume — resume отвечает ошибкой.
	FailResume bool
	// Profile — липкий permission profile треда; пусто — Full Access.
	Profile string
	// IgnorePermissions — resume не меняет профиль и не называет его.
	IgnorePermissions bool
	// ResumeSandbox — какой sandbox.type назвать в ответе resume вместо
	// выведенного из профиля.
	ResumeSandbox string
	// BusyAfterResume — к концу resume тред занят чужим ходом.
	BusyAfterResume bool
	Cwd             string

	// FailSettings — thread/settings/update отвечает ошибкой.
	FailSettings bool
	// NoSettingsNotice — update принят, а thread/settings/updated не приходит.
	NoSettingsNotice bool
	// SettingsNoticeFor — уведомление приходит, но про другой тред.
	SettingsNoticeFor string
	// SettingsNoticeFirst — уведомление обгоняет ответ на update.
	SettingsNoticeFirst bool
	// SettingsSandbox — какой sandboxPolicy.type назвать в уведомлении вместо
	// выведенного из профиля.
	SettingsSandbox string
	// BusyAfterSettings — к концу update тред занят чужим ходом.
	BusyAfterSettings bool
}

type Fake struct {
	Path string
	// OnAdd вызывается на каждый thread/queue/add.
	OnAdd func(thread, text string)
	// Profiles — ответ permissionProfile/list; по умолчанию разрешён один
	// claudex-work.
	Profiles []codex.PermissionProfile
	// FailProfiles — permissionProfile/list отвечает ошибкой.
	FailProfiles bool

	mu      sync.Mutex
	threads map[string]*Thread
	calls   []string
	resumes []map[string]any
	updates []map[string]any
	// ProfileCwds — с каким cwd спрашивали permissionProfile/list.
	ProfileCwds []string
	seq         int
	ln          net.Listener
}

// Start поднимает подделку и направляет на неё codex.Dial.
func Start(t *testing.T) *Fake {
	t.Helper()
	// Короткий путь: у unix-сокета на macOS предел в 104 байта, а t.TempDir()
	// длиннее.
	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &Fake{Path: filepath.Join(dir, "s.sock"), threads: map[string]*Thread{},
		Profiles: []codex.PermissionProfile{{ID: codex.WakePermissionProfile, Allowed: true}}}
	f.ln, err = net.Listen("unix", f.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.ln.Close() })
	go f.serve()
	t.Setenv("CLAUDEX_CODEX_SOCKET", f.Path)
	return f
}

func (f *Fake) Add(id string, th *Thread) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if th.Status.Type == "" {
		th.Status.Type = codex.StatusNotLoaded
	}
	f.threads[id] = th
}

func (f *Fake) Thread(id string) Thread {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := *f.threads[id]
	th.Queue = append([]codex.QueuedItem(nil), th.Queue...)
	th.Turns = append([]codex.Turn(nil), th.Turns...)
	return th
}

// Set меняет тред под замком.
func (f *Fake) Set(id string, change func(*Thread)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.threads[id])
}

// Finish завершает ход, начатый с KeepRunning.
func (f *Fake) Finish(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	th := f.threads[id]
	for i := range th.Turns {
		if th.Turns[i].Status == codex.TurnInProgress {
			th.Turns[i].Status = status
		}
	}
	th.Status = codex.ThreadStatus{Type: codex.StatusIdle}
}

// Calls — какие методы вызывались, по порядку.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Resumes — параметры каждого thread/resume как они пришли по проводу.
func (f *Fake) Resumes() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.resumes...)
}

// SettingsUpdates — параметры каждого thread/settings/update как они пришли.
func (f *Fake) SettingsUpdates() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.updates...)
}

func (f *Fake) Count(method string) int {
	n := 0
	for _, c := range f.Calls() {
		if c == method {
			n++
		}
	}
	return n
}

func (f *Fake) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.conn(c)
	}
}

func (f *Fake) conn(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	req, err := http.ReadRequest(r)
	if err != nil || !codex.IsUpgrade(req.Header) {
		return
	}
	fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n", codex.AcceptKey(req.Header.Get("Sec-WebSocket-Key")))
	// Пинг до всего прочего: клиент обязан ответить и не сбиться.
	codex.WriteFrame(c, 0x9, []byte("p"), false)
	for {
		_, op, p, err := codex.ReadFrame(r)
		if err != nil || op == 0x8 {
			return
		}
		if op != 0x1 {
			continue
		}
		var m struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(p, &m) != nil || m.ID == nil {
			continue
		}
		res, rpcErr, n := f.handle(m.Method, m.Params)
		// Уведомление и встречный запрос перед ответом — как у живого демона.
		send(c, map[string]any{"method": "thread/status/changed", "params": map[string]any{}})
		if n.v != nil && n.first {
			send(c, n.v)
		}
		if m.Method == "thread/resume" {
			send(c, map[string]any{"id": 900, "method": "item/commandExecution/requestApproval",
				"params": map[string]any{}})
		}
		out := map[string]any{"id": *m.ID}
		if rpcErr != "" {
			out["error"] = map[string]any{"code": -32600, "message": rpcErr}
		} else {
			out["result"] = res
		}
		if m.Method == "thread/turns/list" {
			sendFragmented(c, out)
		} else {
			send(c, out)
		}
		if n.v != nil && !n.first {
			send(c, n.v)
		}
	}
}

// notice — уведомление, которое сервер шлёт вместе с ответом.
type notice struct {
	v     any
	first bool
}

func send(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	codex.WriteFrame(c, 0x1, b, false)
}

func sendFragmented(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	half := len(b) / 2
	hdr := []byte{0x01}
	if half < 126 {
		hdr = append(hdr, byte(half))
	} else {
		hdr = append(hdr, 126, byte(half>>8), byte(half))
	}
	c.Write(append(hdr, b[:half]...))
	codex.WriteFrame(c, 0x0, b[half:], false)
}

func (f *Fake) handle(method string, raw json.RawMessage) (any, string, notice) {
	if method == "thread/settings/update" {
		return f.settingsUpdate(raw)
	}
	res, err := f.reply(method, raw)
	return res, err, notice{}
}

func (f *Fake) reply(method string, raw json.RawMessage) (any, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	var p struct {
		ThreadID string            `json:"threadId"`
		Cwd      string            `json:"cwd"`
		ClientID string            `json:"clientUserMessageId"`
		Input    []codex.UserInput `json:"input"`
		QueueID  string            `json:"queuedSubmissionId"`
	}
	json.Unmarshal(raw, &p)
	if method == "initialize" {
		return map[string]any{}, ""
	}
	if method == "permissionProfile/list" {
		f.ProfileCwds = append(f.ProfileCwds, p.Cwd)
		if f.FailProfiles {
			return nil, "config error"
		}
		return map[string]any{"data": f.Profiles, "nextCursor": nil}, ""
	}
	th := f.threads[p.ThreadID]
	if th == nil {
		return nil, "thread not found: " + p.ThreadID
	}
	switch method {
	case "thread/read":
		return map[string]any{"thread": map[string]any{"id": p.ThreadID, "status": th.Status,
			"ephemeral": th.Ephemeral, "cwd": th.Cwd}}, ""
	case "thread/queue/list":
		return map[string]any{"data": orEmpty(th.Queue), "nextCursor": nil}, ""
	case "thread/queue/add":
		f.seq++
		it := codex.QueuedItem{ID: fmt.Sprintf("q%d", f.seq), ClientID: p.ClientID, Input: p.Input}
		th.Queue = append(th.Queue, it)
		if f.OnAdd != nil {
			f.OnAdd(p.ThreadID, it.Text())
		}
		if th.Status.Type == codex.StatusIdle && !th.Held {
			f.drain(th)
		}
		return map[string]any{"queuedSubmission": it}, ""
	case "thread/resume":
		var o map[string]any
		json.Unmarshal(raw, &o)
		f.resumes = append(f.resumes, o)
		for k := range o {
			if k != "threadId" && k != "excludeTurns" && k != "permissions" {
				return nil, "фальшивка: resume с переопределением " + k
			}
		}
		perm, _ := o["permissions"].(string)
		if !f.allowed(perm) {
			return nil, "unknown or disallowed permission profile: " + perm
		}
		if th.FailResume {
			return nil, "resume failed"
		}
		if !th.IgnorePermissions {
			th.Profile = perm
		}
		if th.Status.Type == codex.StatusNotLoaded {
			th.Status = codex.ThreadStatus{Type: codex.StatusIdle}
			if !th.NoDrainOnResume && !th.Held {
				f.drain(th)
			}
		}
		if th.BusyAfterResume {
			th.Status = codex.ThreadStatus{Type: codex.StatusActive, ActiveFlags: []string{}}
		}
		out := map[string]any{"thread": map[string]any{"id": p.ThreadID, "status": th.Status},
			"sandbox": map[string]any{"type": "dangerFullAccess"}}
		if th.Profile != "" {
			out["activePermissionProfile"] = map[string]any{"id": th.Profile}
			out["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": true}
		}
		if th.ResumeSandbox != "" {
			out["sandbox"] = map[string]any{"type": th.ResumeSandbox}
		}
		return out, ""
	case "thread/queue/start":
		if th.Status.Type != codex.StatusIdle {
			return nil, "thread is not idle"
		}
		for i, it := range th.Queue {
			if it.ID == p.QueueID {
				th.Queue = append(th.Queue[:i], th.Queue[i+1:]...)
				return map[string]any{"turn": f.start(th, it)}, ""
			}
		}
		return nil, "queued submission not found"
	case "thread/turns/list":
		out := make([]codex.Turn, 0, len(th.Turns))
		for i := len(th.Turns) - 1; i >= 0; i-- {
			out = append(out, th.Turns[i])
		}
		return map[string]any{"data": out}, ""
	}
	return nil, "unknown method " + method
}

// settingsUpdate — как thread/settings/update у 0.162.1: ответ пуст, а
// применённые настройки приходят уведомлением thread/settings/updated.
func (f *Fake) settingsUpdate(raw json.RawMessage) (any, string, notice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "thread/settings/update")
	var o map[string]any
	json.Unmarshal(raw, &o)
	f.updates = append(f.updates, o)
	id, _ := o["threadId"].(string)
	th := f.threads[id]
	if th == nil {
		return nil, "thread not found: " + id, notice{}
	}
	for k := range o {
		if k != "threadId" && k != "permissions" {
			return nil, "фальшивка: settings/update с " + k, notice{}
		}
	}
	if th.Status.Type == codex.StatusNotLoaded {
		return nil, "thread not loaded: " + id, notice{}
	}
	perm, _ := o["permissions"].(string)
	if !f.allowed(perm) {
		return nil, "unknown or disallowed permission profile: " + perm, notice{}
	}
	if th.FailSettings {
		return nil, "settings update failed", notice{}
	}
	if !th.IgnorePermissions {
		th.Profile = perm
	}
	if th.BusyAfterSettings {
		th.Status = codex.ThreadStatus{Type: codex.StatusActive, ActiveFlags: []string{}}
	}
	if th.NoSettingsNotice {
		return map[string]any{}, "", notice{}
	}
	settings := map[string]any{"cwd": th.Cwd, "model": "gpt-test", "modelProvider": "openai",
		"approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}}
	if th.Profile != "" {
		settings["activePermissionProfile"] = map[string]any{"id": th.Profile}
		settings["sandboxPolicy"] = map[string]any{"type": "workspaceWrite", "networkAccess": true}
	}
	if th.SettingsSandbox != "" {
		settings["sandboxPolicy"] = map[string]any{"type": th.SettingsSandbox}
	}
	to := id
	if th.SettingsNoticeFor != "" {
		to = th.SettingsNoticeFor
	}
	return map[string]any{}, "", notice{first: th.SettingsNoticeFirst, v: map[string]any{
		"method": "thread/settings/updated",
		"params": map[string]any{"threadId": to, "threadSettings": settings}}}
}

func (f *Fake) allowed(id string) bool {
	for _, p := range f.Profiles {
		if p.ID == id && p.Allowed {
			return true
		}
	}
	return false
}

func (f *Fake) drain(th *Thread) {
	for len(th.Queue) > 0 && th.Status.Type == codex.StatusIdle {
		it := th.Queue[0]
		th.Queue = th.Queue[1:]
		f.start(th, it)
	}
}

func (f *Fake) start(th *Thread, it codex.QueuedItem) codex.Turn {
	f.seq++
	t := codex.Turn{ID: fmt.Sprintf("t%d", f.seq), Status: codex.TurnCompleted,
		Items: []codex.TurnItem{{Type: "userMessage", ClientID: it.ClientID}}}
	if th.KeepRunning {
		t.Status = codex.TurnInProgress
		th.Status = codex.ThreadStatus{Type: codex.StatusActive, ActiveFlags: []string{}}
	}
	th.Turns = append(th.Turns, t)
	return t
}

func orEmpty(q []codex.QueuedItem) []codex.QueuedItem {
	if q == nil {
		return []codex.QueuedItem{}
	}
	return q
}
