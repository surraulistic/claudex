package claudesess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain отвязывает набор от сессии, в которой его запускают.
//
// Иначе тесты читают живое окружение разработчика: claudex собирают изнутри
// Claude Code, и CLAUDE_CODE_SESSION_ID там выставлен. Первый же прогон это и
// показал — идентификатор в фикстуре совпал с идентификатором запускавшей
// сессии, и выбор адресата отказал как «это текущая сессия». Набор, который
// проходит или падает от того, кто его запустил, не доказывает ничего.
func TestMain(m *testing.M) {
	for _, k := range []string{
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN",
		"CLAUDE_CONFIG_DIR",
	} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// Форма записи снята с живого реестра 26.09: ровно те имена полей, которые
// пишет Claude Code. Выдумывать её нельзя — на выдуманной схеме разбор
// зелёный, а на настоящей адресат не находится.
const liveRecord = `{
  "pid": 60659,
  "sessionId": "81fad209-733a-4b55-b5f2-6a8b9cc14eed",
  "cwd": "/Users/surraulistic/GolandProjects",
  "startedAt": 1789501355691,
  "version": "2.1.258",
  "peerProtocol": 1,
  "kind": "bg",
  "entrypoint": "cli",
  "messagingSocketPath": "%s",
  "name": "Performance review claudex (2)",
  "updatedAt": 1790388484454,
  "status": "busy",
  "statusUpdatedAt": 1790388484454
}`

// socketDir — каталог под сокеты с коротким путём.
//
// Длина пути юникс-сокета ограничена сотней с небольшим байт, а t.TempDir()
// на macOS выдаёт /var/folders/<...> и один только он уже съедает половину.
// Тест на этом падает не по существу.
func socketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// listen поднимает сокет и возвращает канал, в который кладёт всё полученное.
func listen(t *testing.T, path string) <-chan string {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan string, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b := make([]byte, 64<<10)
			n, _ := c.Read(b)
			got <- string(b[:n])
			c.Close()
		}
	}()
	return got
}

// registry кладёт в каталог записи реестра с живыми сокетами.
func registry(t *testing.T, sessions ...Session) string {
	t.Helper()
	home, sock := t.TempDir(), socketDir(t)
	for _, s := range sessions {
		p := filepath.Join(sock, fmt.Sprintf("%d.sock", s.PID))
		if s.Socket == "" {
			s.Socket = p
			listen(t, p)
		}
		rec := map[string]any{
			"pid": s.PID, "sessionId": s.ID, "cwd": s.CWD, "kind": s.Kind,
			"name": s.Name, "status": s.Status, "messagingSocketPath": s.Socket,
			"statusUpdatedAt": time.Now().UnixMilli(),
		}
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(home, fmt.Sprintf("%d.json", s.PID)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestLiveRecordIsParsed(t *testing.T) {
	home, sock := t.TempDir(), socketDir(t)
	p := filepath.Join(sock, "60659.sock")
	listen(t, p)
	if err := os.WriteFile(filepath.Join(home, "60659.json"),
		[]byte(fmt.Sprintf(liveRecord, p)), 0o600); err != nil {
		t.Fatal(err)
	}

	got := List(home)
	if len(got) != 1 {
		t.Fatalf("одна сессия, получено %d", len(got))
	}
	s := got[0]
	if s.ID != "81fad209-733a-4b55-b5f2-6a8b9cc14eed" || s.PID != 60659 {
		t.Fatalf("разговор и pid разобраны, получено %+v", s)
	}
	if s.Name != "Performance review claudex (2)" || s.Kind != "bg" {
		t.Fatalf("имя и вид разобраны, получено %+v", s)
	}
	if !s.Busy() {
		t.Fatalf("занятость разобрана, получено %q", s.Status)
	}
	if s.Socket != p {
		t.Fatalf("адрес взят из записи, получено %q", s.Socket)
	}
}

func TestRecordWithoutASocketIsNotLive(t *testing.T) {
	// Записи переживают свои процессы: в живом реестре их было 36 при 18
	// живых сокетах. Запись без сокета — это не адрес, и выдавать её за
	// живую значит обещать доставку туда, где никто не слушает.
	home := t.TempDir()
	b, _ := json.Marshal(map[string]any{
		"pid": 111, "sessionId": "dead", "name": "ушедшая",
		"messagingSocketPath": "/tmp/cs-нет-такого/111.sock", "status": "idle",
	})
	os.WriteFile(filepath.Join(home, "111.json"), b, 0o600)

	if got := List(home); len(got) != 0 {
		t.Fatalf("мёртвая запись отброшена, получено %+v", got)
	}
}

func TestResolveByConversationId(t *testing.T) {
	home := registry(t,
		Session{PID: 1, ID: "81fad209-733a-4b55-b5f2-6a8b9cc14eed", Name: "один", Status: "idle"},
		Session{PID: 2, ID: "44d5f44b-e510-4236-9344-e72e5e7377f4", Name: "два", Status: "idle"},
	)
	for _, target := range []string{"81fad209-733a-4b55-b5f2-6a8b9cc14eed", "81fad209"} {
		got, err := Resolve(home, target)
		if err != nil {
			t.Fatalf("%q: %v", target, err)
		}
		if got.PID != 1 {
			t.Fatalf("%q указывает на первую, получено %+v", target, got)
		}
	}
}

func TestResolveByUniqueName(t *testing.T) {
	home := registry(t,
		Session{PID: 1, ID: "aaaaaaaa-0000-0000-0000-000000000000", Name: "license", Status: "idle"},
		Session{PID: 2, ID: "bbbbbbbb-0000-0000-0000-000000000000", Name: "casino-toolkit", Status: "idle"},
	)
	got, err := Resolve(home, "license")
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 1 {
		t.Fatalf("имя ведёт к своей сессии, получено %+v", got)
	}
}

func TestSharedNameIsRefusedWithTheCandidatesNamed(t *testing.T) {
	// В живом реестре три сессии звались «license». Выбрать за человека
	// нельзя: промах уводит поручение в чужой разговор, а это ровно та
	// ошибка, ради которой адресация по разговору и заводилась.
	home := registry(t,
		Session{PID: 1, ID: "aaaaaaaa-0000-0000-0000-000000000000", Name: "license",
			CWD: "/a", Status: "idle"},
		Session{PID: 2, ID: "bbbbbbbb-0000-0000-0000-000000000000", Name: "license",
			CWD: "/b", Status: "idle"},
	)
	_, err := Resolve(home, "license")
	var amb *Ambiguous
	if !errors.As(err, &amb) {
		t.Fatalf("отказ по неоднозначности, получено %v", err)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("названы оба, получено %+v", amb.Candidates)
	}
	for _, want := range []string{"aaaaaaaa", "bbbbbbbb", "/a", "/b"} {
		if !strings.Contains(amb.Error(), want) {
			t.Fatalf("в отказе есть %q, получено %q", want, amb.Error())
		}
	}
}

func TestUnknownTargetIsItsOwnRefusal(t *testing.T) {
	// «Нет такой» и «их несколько» — разные беды, и лечатся по-разному.
	home := registry(t, Session{PID: 1, ID: "aaaaaaaa-0000-0000-0000-000000000000",
		Name: "license", Status: "idle"})
	if _, err := Resolve(home, "payments"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("отказ «нет такой», получено %v", err)
	}
}

func TestOwnSessionIsNotAnAddress(t *testing.T) {
	// Claude Code отказывает в сообщении самому себе, и claudex не должен
	// доводить до этого отказа: свой же разговор адресом не бывает.
	const self = "81fad209-733a-4b55-b5f2-6a8b9cc14eed"
	t.Setenv("CLAUDE_CODE_SESSION_ID", self)
	home := registry(t, Session{PID: 1, ID: self, Name: "я", Status: "busy"})
	if _, err := Resolve(home, "я"); !errors.Is(err, ErrSelf) {
		t.Fatalf("свой разговор отклонён, получено %v", err)
	}
}

func TestSendWritesTheDocumentedFrame(t *testing.T) {
	// Форма взята из образца в самом Claude Code и проверена на живой
	// сессии: сообщение дошло. Менять её произвольно нельзя.
	dir := socketDir(t)
	p := filepath.Join(dir, "1.sock")
	got := listen(t, p)

	err := Send(context.Background(), Session{PID: 1, Socket: p}, "перебазируйся на main")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-got:
		lines := strings.Split(strings.TrimSpace(raw), "\n")
		if len(lines) != 1 {
			t.Fatalf("одна строка без ключа: адресат чужой, наш ключ ему не годится; получено %d: %q", len(lines), raw)
		}
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &f); err != nil {
			t.Fatalf("строка разбирается как JSON, получено %q", lines[0])
		}
		if f.Type != "user" || f.Message.Role != "user" {
			t.Fatalf("вид кадра, получено %+v", f)
		}
		if f.Message.Content != "перебазируйся на main" {
			t.Fatalf("текст уехал как есть, получено %q", f.Message.Content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("сокет получил сообщение")
	}
}

func TestOwnSocketAlsoGetsTheAuthLine(t *testing.T) {
	// Ключ сессии годится ровно для её собственного ящика: им Claude Code
	// опознаёт своего потомка, когда доказательств по процессу уже нет.
	// Чужому адресату он бессмыслен, и слать его туда — шум.
	dir := socketDir(t)
	p := filepath.Join(dir, "2.sock")
	got := listen(t, p)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", p)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "3318c40f33f5d1a9")

	if err := Send(context.Background(), Session{PID: 2, Socket: p}, "привет"); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-got:
		lines := strings.Split(strings.TrimSpace(raw), "\n")
		if len(lines) != 2 {
			t.Fatalf("ключ и сообщение, получено %d: %q", len(lines), raw)
		}
		var a struct {
			Type, Token string
		}
		json.Unmarshal([]byte(lines[0]), &a)
		if a.Type != "auth" || a.Token != "3318c40f33f5d1a9" {
			t.Fatalf("первой строкой ключ, получено %q", lines[0])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("сокет получил сообщение")
	}
}

func TestForeignSocketNeverGetsOurToken(t *testing.T) {
	// Ключ выставлен, но адресат чужой. Ловится только так: без выставленного
	// ключа мутация «слать его всем» проходит незамеченной — сам зонд это и
	// показал.
	dir := socketDir(t)
	mine := filepath.Join(dir, "4.sock")
	theirs := filepath.Join(dir, "5.sock")
	listen(t, mine)
	got := listen(t, theirs)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", mine)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "секрет-нашей-сессии")

	if err := Send(context.Background(), Session{PID: 5, Socket: theirs}, "текст"); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-got:
		if strings.Contains(raw, "секрет-нашей-сессии") || strings.Contains(raw, `"auth"`) {
			t.Fatalf("чужому ключ не уехал, получено %q", raw)
		}
		if n := len(strings.Split(strings.TrimSpace(raw), "\n")); n != 1 {
			t.Fatalf("одна строка, получено %d: %q", n, raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("сокет получил сообщение")
	}
}

func TestSendToADeadSocketIsAnError(t *testing.T) {
	// Сессия умирает между чтением реестра и записью. Молчаливый успех тут
	// хуже отказа: отчёт считался бы доставленным и выпал бы из дожима.
	dir := socketDir(t)
	p := filepath.Join(dir, "3.sock")
	listen(t, p)
	os.Remove(p)

	if err := Send(context.Background(), Session{PID: 3, Socket: p}, "текст"); err == nil {
		t.Fatal("пропавший адресат — ошибка, а не успех")
	}
}

func TestEmptyTargetIsRefusedBeforeTheRegistryIsRead(t *testing.T) {
	if _, err := Resolve(t.TempDir(), "  "); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("пустой адресат назван, получено %v", err)
	}
}

// setStatus переписывает занятость в записи реестра: так её меняет и сам
// Claude Code — переписыванием файла целиком.
func setStatus(t *testing.T, home string, pid int, status string) {
	t.Helper()
	p := filepath.Join(home, fmt.Sprintf("%d.json", pid))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	m["status"] = status
	m["statusUpdatedAt"] = time.Now().UnixMilli()
	nb, _ := json.Marshal(m)
	if err := os.WriteFile(p, nb, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWaitIdleReturnsWhenTheWorkIsDone(t *testing.T) {
	// Разговор берётся за работу и освобождается. Именно эта пара переходов и
	// означает «поручение доведено до конца».
	home := registry(t, Session{PID: 1, ID: "aaaaaaaa-0000-0000-0000-000000000000",
		Name: "worker", Status: "idle"})
	go func() {
		time.Sleep(40 * time.Millisecond)
		setStatus(t, home, 1, "busy")
		time.Sleep(80 * time.Millisecond)
		setStatus(t, home, 1, "idle")
	}()
	err := WaitIdle(context.Background(), home, "aaaaaaaa-0000-0000-0000-000000000000",
		10*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("дождались завершения, получено %v", err)
	}
}

func TestIdleBeforePickupIsNotMistakenForDone(t *testing.T) {
	// Разговор простаивает с самого начала: он ещё не брался за поручение.
	// Считать это завершением значит выдать несделанное за сделанное — ровно
	// та ошибка, из-за которой «панель свободна» перестали считать
	// доказательством.
	home := registry(t, Session{PID: 1, ID: "aaaaaaaa-0000-0000-0000-000000000000",
		Name: "worker", Status: "idle"})
	err := WaitIdle(context.Background(), home, "aaaaaaaa-0000-0000-0000-000000000000",
		10*time.Millisecond, 120*time.Millisecond)
	if !errors.Is(err, ErrNoPickup) {
		t.Fatalf("незанятость названа своей причиной, получено %v", err)
	}
}

func TestConversationVanishingMidTaskIsAnError(t *testing.T) {
	// Сессию закрыли на полпути. Молчаливый успех тут хуже ошибки: поручение
	// считалось бы выполненным.
	home := registry(t, Session{PID: 1, ID: "aaaaaaaa-0000-0000-0000-000000000000",
		Name: "worker", Status: "busy"})
	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Remove(filepath.Join(home, "1.json"))
	}()
	err := WaitIdle(context.Background(), home, "aaaaaaaa-0000-0000-0000-000000000000",
		10*time.Millisecond, time.Second)
	if err == nil || errors.Is(err, ErrNoPickup) {
		t.Fatalf("пропажа разговора — ошибка, получено %v", err)
	}
}

func TestLookupReachesOwnConversationWhereResolveRefusesIt(t *testing.T) {
	// Два разных вопроса. Человек, назвавший свой же разговор, ошибся — ему
	// отказ. Адрес из журнала при доставке ошибкой не является: запись в
	// собственный ящик Claude Code поддерживает, а дожим зависших отчётов
	// выполняется при любом вызове claudex, в том числе изнутри адресата.
	// Отказ там оставил бы отчёт висеть навсегда.
	const self = "81fad209-733a-4b55-b5f2-6a8b9cc14eed"
	t.Setenv("CLAUDE_CODE_SESSION_ID", self)
	home := registry(t, Session{PID: 1, ID: self, Name: "я", Status: "busy"})

	if _, err := Resolve(home, self); !errors.Is(err, ErrSelf) {
		t.Fatalf("названный человеком свой разговор отклонён, получено %v", err)
	}
	got, err := Lookup(home, self)
	if err != nil {
		t.Fatalf("доставка в свой разговор проходит, получено %v", err)
	}
	if got.ID != self {
		t.Fatalf("нашёлся именно он, получено %+v", got)
	}
}

func TestLookupStillRefusesTheUnknownAndTheAmbiguous(t *testing.T) {
	// Послабление касается только собственного разговора: остальные отказы
	// доставке нужны ровно так же.
	home := registry(t,
		Session{PID: 1, ID: "aaaaaaaa-0000-0000-0000-000000000000", Name: "license", Status: "idle"},
		Session{PID: 2, ID: "bbbbbbbb-0000-0000-0000-000000000000", Name: "license", Status: "idle"},
	)
	if _, err := Lookup(home, "payments"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("неизвестный отклонён, получено %v", err)
	}
	var amb *Ambiguous
	if _, err := Lookup(home, "license"); !errors.As(err, &amb) {
		t.Fatalf("неоднозначный отклонён, получено %v", err)
	}
}
