//go:build codexlive

package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Живой smoke обоих маршрутов на одноразовых тредах. Стоит двух коротких ходов
// модели. Запуск:
//
//	CLAUDEX_SMOKE_DIR=<пустой каталог> go test -tags codexlive -run TestLive -v ./internal/codex

const smokeInstructions = "Это одноразовый тестовый тред проверки доставки claudex. " +
	"На любое сообщение ответь одним словом OK. Никаких инструментов и команд."

type stdioOwner struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	mu   sync.Mutex
	next int
	wait map[int]chan json.RawMessage
}

func startOwner(t *testing.T, bin string) *stdioOwner {
	t.Helper()
	// Команда runtime ChatGPT Desktop: у CodexCLI 0.167.0-alpha.3.2 это
	// `app-server --listen stdio://` (снято 12.11 с живого процесса); прежняя
	// форма с `-c features.code_mode_host=true` отличалась только флагами.
	cmd := exec.Command(bin, "app-server", "--listen", "stdio://")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	o := &stdioOwner{cmd: cmd, in: in, wait: map[int]chan json.RawMessage{}}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var r struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID == nil || r.Method != "" {
				continue
			}
			o.mu.Lock()
			ch := o.wait[*r.ID]
			o.mu.Unlock()
			if ch != nil {
				if r.Error != nil {
					ch <- append(json.RawMessage(`{"__error":`), append(r.Error, '}')...)
				} else {
					ch <- r.Result
				}
			}
		}
	}()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return o
}

func (o *stdioOwner) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	o.mu.Lock()
	o.next++
	id := o.next
	ch := make(chan json.RawMessage, 1)
	o.wait[id] = ch
	o.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if _, err := o.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(90 * time.Second):
		t.Fatalf("владелец не ответил на %s", method)
	}
	return nil
}

func (o *stdioOwner) notify(method string) {
	b, _ := json.Marshal(map[string]any{"method": method})
	o.in.Write(append(b, '\n'))
}

func startParams(dir string) map[string]any {
	return map[string]any{"cwd": dir, "permissions": WakePermissionProfile, "approvalPolicy": "never",
		"ephemeral": false, "developerInstructions": smokeInstructions}
}

func injectParams(id string) map[string]any {
	return map[string]any{"threadId": id, "items": []any{map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "smoke: тред создан для проверки доставки"}}}}}
}

func threadID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Thread.ID == "" {
		t.Fatalf("thread/start: %s", raw)
	}
	return r.Thread.ID
}

func smokeDir(t *testing.T) string {
	d := os.Getenv("CLAUDEX_SMOKE_DIR")
	if d == "" {
		t.Skip("CLAUDEX_SMOKE_DIR не задан")
	}
	// TestMain пакета уводит сокет и бинарь в никуда; живому smoke нужны настоящие.
	os.Unsetenv("CLAUDEX_CODEX_SOCKET")
	os.Unsetenv("CLAUDEX_CODEX_BIN")
	return d
}

func followDone(t *testing.T, thread, clientID string, since time.Time) WakeResult {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		r, err := Follow(context.Background(), thread, clientID, since, 10*time.Second)
		if err == nil && (r.Stage == StageCompleted || r.Stage == StageTurnFailed) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("ход не завершился: %+v %v", r, err)
		}
		time.Sleep(3 * time.Second)
	}
}

func TestLiveDesktopOwnerRoute(t *testing.T) {
	dir := smokeDir(t)
	bin := os.Getenv("CLAUDEX_SMOKE_OWNER_BIN")
	if bin == "" {
		bin = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"
	}
	own := startOwner(t, bin)
	own.call(t, "initialize", map[string]any{"clientInfo": map[string]string{"name": "claudex-smoke-owner", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true}})
	own.notify("initialized")
	id := threadID(t, own.call(t, "thread/start", startParams(dir)))
	t.Logf("одноразовый тред владельца: %s (pid %d)", id, own.cmd.Process.Pid)
	t.Cleanup(func() { t.Logf("archive: %s", own.call(t, "thread/archive", map[string]any{"threadId": id})) })
	t.Logf("inject: %s", own.call(t, "thread/inject_items", injectParams(id)))

	var o Owner
	for i := 0; i < 20; i++ {
		if o = ThreadOwner(Home(), id); o.Kind != OwnerNone {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("владелец: kind=%s %s why=%q", o.Kind, o, o.Why)
	if o.Kind != OwnerAppServer || o.PID != own.cmd.Process.Pid {
		t.Fatalf("владелец — наш stdio app-server: %+v", o)
	}

	since := time.Now()
	req := WakeRequest{Thread: id, ClientID: "claudex-smoke0b-reported", Task: "smoke0b",
		Text:  TextPrefix + "smoke0b — проверка доставки владельцу треда. Ответь одним словом OK.",
		Owner: func() Owner { return ThreadOwner(Home(), id) }, Since: since,
		StartWait: 45 * time.Second, Poll: time.Second, Each: 10 * time.Second}
	start := time.Now()
	r, err := Wake(context.Background(), req)
	t.Logf("Wake за %s: stage=%s hold=%s transport=%s turn=%s reason=%q err=%v",
		time.Since(start).Round(time.Millisecond), r.Stage, r.Hold, r.Transport, r.TurnID, r.Reason, err)
	if err != nil || r.Transport != TransportOwnerQueue {
		t.Fatalf("маршрут владельца: %+v %v", r, err)
	}
	if !r.Started() {
		req.Accepted = true
		time.Sleep(15 * time.Second)
		r, err = Wake(context.Background(), req)
		t.Logf("повтор: stage=%s hold=%s err=%v", r.Stage, r.Hold, err)
	}
	if !r.Started() {
		t.Fatalf("ход владельца не начался: %+v", r)
	}
	for i := 0; i < 20; i++ {
		d, err := Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		turns, _, err := d.TurnsPage(context.Background(), id, "", 3, ItemsSummary)
		d.Close()
		var view []string
		for _, tr := range turns {
			view = append(view, tr.ID[len(tr.ID)-6:]+":"+tr.Status+":done="+strconv.FormatInt(tr.CompletedAt, 10))
		}
		ownRaw := own.call(t, "thread/turns/list", map[string]any{"threadId": id, "limit": 3, "itemsView": "summary"})
		var ov struct {
			Data []Turn `json:"data"`
		}
		json.Unmarshal(ownRaw, &ov)
		var oview []string
		for _, tr := range ov.Data {
			oview = append(oview, tr.ID[len(tr.ID)-6:]+":"+tr.Status+":done="+strconv.FormatInt(tr.CompletedAt, 10))
		}
		t.Logf("t+%2ds демон видит %v · владелец видит %v err=%v", i*3, view, oview, err)
		if len(ov.Data) > 0 && ov.Data[0].Status == TurnCompleted && len(turns) > 0 && turns[0].Status == TurnCompleted {
			break
		}
		time.Sleep(3 * time.Second)
	}
	done := followDone(t, id, req.ClientID, since)
	t.Logf("итог: stage=%s turn=%s status=%s", done.Stage, done.TurnID, done.TurnStatus)

	s, err := Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	th, err := s.ReadThread(context.Background(), id)
	t.Logf("демон (pid %d) видит тред: %+v err=%v", s.PeerPID(), th.Status, err)
	if th.Status.Type != StatusNotLoaded {
		t.Fatalf("демон тред не загружал: %+v", th.Status)
	}
	if o2 := ThreadOwner(Home(), id); o2.PID != own.cmd.Process.Pid {
		t.Fatalf("замок по-прежнему у владельца: %+v", o2)
	}
}

func TestLiveDaemonRoute(t *testing.T) {
	dir := smokeDir(t)
	s, err := Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cl := s.(*client)
	var raw json.RawMessage
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := cl.call(ctx, "thread/start", startParams(dir), &raw); err != nil {
		t.Fatal(err)
	}
	id := threadID(t, raw)
	if err := cl.call(ctx, "thread/inject_items", injectParams(id), nil); err != nil {
		t.Fatal(err)
	}
	peer := s.PeerPID()
	s.Close()
	t.Logf("одноразовый тред демона %d: %s", peer, id)
	t.Cleanup(func() {
		s, err := Dial(context.Background())
		if err != nil {
			t.Log(err)
			return
		}
		defer s.Close()
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		t.Logf("archive: %v", s.(*client).call(c, "thread/archive", map[string]any{"threadId": id}, nil))
	})

	since := time.Now()
	req := WakeRequest{Thread: id, ClientID: "claudex-smoke0a-reported", Task: "smoke0a",
		Text:  TextPrefix + "smoke0a — проверка доставки через демон. Ответь одним словом OK.",
		Owner: func() Owner { return ThreadOwner(Home(), id) }, Since: since,
		StartWait: 45 * time.Second, Poll: time.Second, Each: 10 * time.Second}
	r, err := Wake(context.Background(), req)
	t.Logf("Wake: stage=%s hold=%s transport=%s profile=%s turn=%s reason=%q err=%v",
		r.Stage, r.Hold, r.Transport, r.ProfileSet, r.TurnID, r.Reason, err)
	if err != nil || !r.Started() || r.Transport != TransportAppServer {
		t.Fatalf("маршрут демона: %+v %v", r, err)
	}
	done := followDone(t, id, req.ClientID, since)
	t.Logf("итог: stage=%s turn=%s status=%s", done.Stage, done.TurnID, done.TurnStatus)
}

// Холодный маршрут: тред демона выгружен, подписчиков и замка нет. Так
// выглядит тред Desktop после закрытия приложения и тред remote-control после
// ухода телефона: демон выгружает его через thread_unload_delay_secs (60 с).
func TestLiveColdRoute(t *testing.T) {
	dir := smokeDir(t)
	s, err := Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cl := s.(*client)
	var raw json.RawMessage
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := cl.call(ctx, "thread/start", startParams(dir), &raw); err != nil {
		t.Fatal(err)
	}
	id := threadID(t, raw)
	if err := cl.call(ctx, "thread/inject_items", injectParams(id), nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	left := time.Now()
	t.Logf("одноразовый тред %s: подписчик ушёл, ждём выгрузки", id)
	t.Cleanup(func() {
		s, err := Dial(context.Background())
		if err != nil {
			t.Log(err)
			return
		}
		defer s.Close()
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		t.Logf("archive: %v", s.(*client).call(c, "thread/archive", map[string]any{"threadId": id}, nil))
	})

	for i := 0; ; i++ {
		d, err := Dial(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		th, err := d.ReadThread(context.Background(), id)
		d.Close()
		if err == nil && th.Status.Type == StatusNotLoaded {
			break
		}
		if i >= 60 {
			t.Fatalf("тред не выгружен за 3 минуты: %+v %v", th.Status, err)
		}
		time.Sleep(3 * time.Second)
	}
	o := ThreadOwner(Home(), id)
	t.Logf("выгружен через %s; владелец: kind=%q why=%q", time.Since(left).Round(time.Second), o.Kind, o.Why)
	if o.Kind != OwnerNone {
		t.Fatalf("после выгрузки замка быть не должно: %+v", o)
	}

	since := time.Now()
	req := WakeRequest{Thread: id, ClientID: "claudex-smoke0c-reported", Task: "smoke0c",
		Text:  TextPrefix + "smoke0c — проверка холодного пробуждения. Ответь одним словом OK.",
		Owner: func() Owner { return ThreadOwner(Home(), id) }, Since: since,
		StartWait: 45 * time.Second, Poll: time.Second, Each: 10 * time.Second}
	r, err := Wake(context.Background(), req)
	t.Logf("Wake: stage=%s hold=%s transport=%s profile=%s turn=%s reason=%q err=%v",
		r.Stage, r.Hold, r.Transport, r.ProfileSet, r.TurnID, r.Reason, err)
	if err != nil || !r.Started() || r.Transport != TransportAppServer || r.ProfileSet != ProfileByResume {
		t.Fatalf("холодный маршрут: %+v %v", r, err)
	}
	done := followDone(t, id, req.ClientID, since)
	t.Logf("итог: stage=%s turn=%s status=%s", done.Stage, done.TurnID, done.TurnStatus)
}
