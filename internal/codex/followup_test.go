package codex_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/codex"
	"github.com/surraulistic/claudex/internal/codex/codextest"
)

func TestOwnTurnIsFoundBehindManyLaterTurns(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{})
	if r, err := codex.Wake(context.Background(), req()); err != nil || r.Stage != codex.StageCompleted {
		t.Fatalf("ход начат и завершён: %+v %v", r, err)
	}
	f.AddTurns(th, 25)

	r, err := codex.Follow(context.Background(), th, req().ClientID, time.Time{}, time.Second)
	if err != nil || r.Stage != codex.StageCompleted || r.TurnID == "" {
		t.Fatalf("свой ход виден и за 25 чужими: %+v %v", r, err)
	}
	q := req()
	q.Accepted = true
	r, err = codex.Wake(context.Background(), q)
	if err != nil || r.Stage != codex.StageCompleted || r.Hold != "" {
		t.Fatalf("повтор не путает вычитанное с пропавшим: %+v %v", r, err)
	}
	if f.Count("thread/queue/add") != 1 {
		t.Fatalf("сообщение одно: %v", f.Calls())
	}
}

func TestTurnSearchStopsAtTheAcceptanceTime(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{})
	f.AddTurns(th, 400)
	q := req()
	q.Accepted = true
	q.Since = time.Now().Add(time.Hour)
	before := f.TurnPages()
	r, err := codex.Wake(context.Background(), q)
	if err != nil || r.Hold != codex.HoldItemGone {
		t.Fatalf("своего хода нет — сообщение пропало: %+v %v", r, err)
	}
	if n := f.TurnPages() - before; n != 1 {
		t.Fatalf("ходы старше принятия не листаются, страниц %d", n)
	}
}

func TestFailedTurnIsNotCompletion(t *testing.T) {
	for _, status := range []string{codex.TurnFailed, codex.TurnInterrupted} {
		t.Run(status, func(t *testing.T) {
			f := codextest.Start(t)
			f.Add(th, &codextest.Thread{EndStatus: status})
			r, err := codex.Wake(context.Background(), req())
			if err != nil {
				t.Fatal(err)
			}
			if r.Stage != codex.StageTurnFailed || r.TurnStatus != status || r.Processed() {
				t.Fatalf("ход оборвался — событие не обработано: %+v", r)
			}
			r, err = codex.Wake(context.Background(), req())
			if err != nil || r.Stage != codex.StageTurnFailed {
				t.Fatalf("повтор видит тот же оборванный ход: %+v %v", r, err)
			}
			if f.Count("thread/queue/add") != 1 {
				t.Fatalf("повтор с тем же ClientID не кладёт второй раз: %v", f.Calls())
			}
		})
	}
}

func TestStalledAppServerIsBoundedByContext(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{})
	f.StallOn("thread/read")
	q := req()
	q.Each = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := codex.Wake(ctx, q)
	if err == nil {
		t.Fatal("зависший сервер — это ошибка, а не успех")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("срок вызывающего держит и зависший сервер: %s", d)
	}
}

func TestTextAloneDoesNotMakeAQueueItemOurs(t *testing.T) {
	human := codex.QueuedItem{ID: "h1", ClientID: "uuid-human",
		Input: []codex.UserInput{{Type: "text", Text: codex.TextPrefix + "bbbb2222 — посмотри"}}}
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Queue: []codex.QueuedItem{human}})
	r, err := codex.Wake(context.Background(), req())
	if err != nil || r.Hold != codex.HoldForeignQueue {
		t.Fatalf("сообщение человека, похожее на наше, — чужое: %+v %v", r, err)
	}
	if f.Count("thread/resume") != 0 {
		t.Fatalf("тред с чужим сообщением не поднимали: %v", f.Calls())
	}

	f2 := codextest.Start(t)
	f2.Add(th, &codextest.Thread{Queue: []codex.QueuedItem{human}})
	q := req()
	q.QueuedByCLI = []codex.CLIQueued{{Task: "bbbb2222", Hash: codex.TextHash("claudex: поручение bbbb2222 — другое")}}
	if r, err := codex.Wake(context.Background(), q); err != nil || r.Hold != codex.HoldForeignQueue {
		t.Fatalf("начало текста совпало, отпечаток нет — чужое: %+v %v", r, err)
	}
	q.QueuedByCLI = []codex.CLIQueued{{Task: "bbbb2222", Hash: codex.TextHash(human.Text())}}
	if r, err := codex.Wake(context.Background(), q); err != nil || r.Hold == codex.HoldForeignQueue {
		t.Fatalf("запись, которую журнал клал codex queue, — своя: %+v %v", r, err)
	}
}

func TestUnconfirmedSwitchAfterRevertIsNotRepeated(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle}, Watched: true,
		Profile: ":workspace", NoSettingsNotice: true})
	if r, _ := codex.Wake(context.Background(), req()); r.ProfileSet != codex.ProfileByUpdateUnconfirmed {
		t.Fatalf("update без подтверждения — возможная смена прав: %+v", r)
	}
	f2 := codextest.Start(t)
	f2.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle}, Watched: true,
		Profile: ":workspace", FailSettings: true})
	if r, _ := codex.Wake(context.Background(), req()); r.ProfileSet != "" {
		t.Fatalf("явный отказ сервера — права не менялись: %+v", r)
	}
}

func TestProfileSwitchIsReportedAndNotRepeatedAfterRevert(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle}, Watched: true,
		Profile: ":workspace"})
	var text string
	f.OnAdd = func(_, msg string) { text = msg }
	r, err := codex.Wake(context.Background(), req())
	if err != nil || r.ProfileSet != codex.ProfileByUpdate || r.ProfileFrom != ":workspace" {
		t.Fatalf("смена прав открытой сессии названа: %+v %v", r, err)
	}
	if !strings.Contains(text, codex.WakePermissionProfile) || !strings.Contains(text, ":workspace") {
		t.Fatalf("человек видит смену прав в самом сообщении, получено %q", text)
	}

	f.Set(th, func(x *codextest.Thread) { x.Profile = ":workspace" })
	q := req()
	q.ClientID, q.KeepProfile = "claudex-aaaa1111-finished", true
	r, err = codex.Wake(context.Background(), q)
	if err != nil || r.Hold != codex.HoldProfileReverted || r.Stage != codex.StageNotQueued {
		t.Fatalf("права вернули — второй раз не переключаем: %+v %v", r, err)
	}
	if f.Count("thread/settings/update") != 1 || f.Count("thread/queue/add") != 1 {
		t.Fatalf("ни второго update, ни второго сообщения: %v", f.Calls())
	}
}

func TestDropAfterAppliedSettingsIsHeldAndConfirmedNextTime(t *testing.T) {
	f := codextest.Start(t)
	f.Add(th, &codextest.Thread{Status: codex.ThreadStatus{Type: codex.StatusIdle}, Watched: true,
		Profile: ":workspace", DropAfterSettings: true})
	r, err := codex.Wake(context.Background(), req())
	if err != nil || r.Hold != codex.HoldLoadedProfile || r.Stage != codex.StageNotQueued ||
		r.ProfileSet != codex.ProfileByUpdateUnconfirmed {
		t.Fatalf("обрыв посреди update — ничего не кладём, попытка названа: %+v %v", r, err)
	}
	f.Set(th, func(x *codextest.Thread) { x.DropAfterSettings = false })
	r, err = codex.Wake(context.Background(), req())
	if err != nil || !r.Processed() {
		t.Fatalf("применённое подтверждает resume: %+v %v", r, err)
	}
	if f.Count("thread/settings/update") != 1 || f.Count("thread/queue/add") != 1 {
		t.Fatalf("второй update не нужен, сообщение одно: %v", f.Calls())
	}
}
