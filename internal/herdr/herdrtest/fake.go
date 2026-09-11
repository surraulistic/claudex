// Package herdrtest поднимает поддельный herdr на настоящем unix-сокете.
// Мок бы отражал мои представления о кадрировании, а ошибиться в них легко:
// сервер берёт один запрос на соединение, но один запрос может нести много
// подписок, и тогда соединение остаётся жить и в него текут события.
package herdrtest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type Fake struct {
	Path string

	mu       sync.Mutex
	replies  map[string]string
	requests []string
	subs     []net.Conn
	opened   chan struct{}
}

// Start поднимает сервер с разумными ответами по умолчанию.
func Start(t *testing.T) *Fake {
	t.Helper()
	// Путь юникс-сокета обрезается на 104 байтах, а t.TempDir() подставляет имя
	// теста — на длинных именах bind падает с invalid argument.
	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	f := &Fake{
		Path:   filepath.Join(dir, "h.sock"),
		opened: make(chan struct{}, 64),
		replies: map[string]string{
			"events.subscribe": `{"id":"s","result":{"type":"subscription_started"}}`,
			"agent.list":       `{"id":"x","result":{"type":"agent_list","agents":[]}}`,
		},
	}
	ln, err := net.Listen("unix", f.Path)
	if err != nil {
		t.Fatalf("сокет: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *Fake) serve(c net.Conn) {
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		c.Close()
		return
	}
	var req struct {
		Method string `json:"method"`
	}
	json.Unmarshal([]byte(line), &req)

	f.mu.Lock()
	f.requests = append(f.requests, line)
	reply, ok := f.replies[req.Method]
	if !ok {
		reply = `{"id":"x","error":{"code":"invalid_request","message":"unknown variant"}}`
	}
	if req.Method == "events.subscribe" {
		f.subs = append(f.subs, c)
	}
	f.mu.Unlock()

	c.Write([]byte(reply + "\n"))
	if req.Method == "events.subscribe" {
		select {
		case f.opened <- struct{}{}:
		default:
		}
		return // подписка держит соединение открытым
	}
	c.Close() // один запрос на соединение
}

func (f *Fake) Reply(method, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method] = raw
}

func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// LastRequest возвращает последний запрос указанного метода.
func (f *Fake) LastRequest(method string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		var r map[string]any
		if json.Unmarshal([]byte(f.requests[i]), &r) == nil && r["method"] == method {
			return r
		}
	}
	return nil
}

func (f *Fake) Push(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.subs {
		c.Write([]byte(line + "\n"))
	}
}

// DropSubscriptions рвёт открытые подписки, как это делает перезапуск herdr.
func (f *Fake) DropSubscriptions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.subs {
		c.Close()
	}
	f.subs = nil
}

// WaitSubscriptions ждёт, пока подписку откроют n раз с начала работы.
func (f *Fake) WaitSubscriptions(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.opened:
		case <-time.After(3 * time.Second):
			t.Fatalf("подписку открыли %d раз из %d", i, n)
		}
	}
}
