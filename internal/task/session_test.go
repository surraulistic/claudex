package task

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
)

const workerSession = "44d5f44b-e510-4236-9344-e72e5e7377f4"

// claudeHome заводит реестр сессий Claude Code с живыми сокетами и возвращает
// канал полученного каждой из них.
func claudeHome(t *testing.T, ids ...string) map[string]<-chan string {
	t.Helper()
	cfg := t.TempDir()
	home := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	// Путь юникс-сокета ограничен сотней байт, а t.TempDir() на macOS уже
	// съедает половину — сокеты живут отдельно, в коротком каталоге.
	sockDir, err := os.MkdirTemp("/tmp", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	out := map[string]<-chan string{}
	for i, id := range ids {
		pid := 1000 + i
		sock := filepath.Join(sockDir, fmt.Sprintf("%d.sock", pid))
		l, err := net.Listen("unix", sock)
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
				b := make([]byte, 32<<10)
				n, _ := c.Read(b)
				got <- string(b[:n])
				c.Close()
			}
		}()
		out[id] = got

		rec, _ := json.Marshal(map[string]any{
			"pid": pid, "sessionId": id, "name": "worker" + fmt.Sprint(i),
			"cwd": "/w", "kind": "bg", "status": "idle", "messagingSocketPath": sock,
			"statusUpdatedAt": time.Now().UnixMilli(),
		})
		if err := os.WriteFile(filepath.Join(home, fmt.Sprintf("%d.json", pid)), rec, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func sessionJournal(t *testing.T) *journal.Journal {
	t.Helper()
	return journal.Open(filepath.Join(t.TempDir(), "tasks.jsonl"))
}

func TestReportReachesTheConversationNotThePane(t *testing.T) {
	// Ради этого третий адрес и заводился: панель переживает смену агента, и
	// «панель свободна» разговора не доказывает. Сокет и есть разговор.
	inbox := claudeHome(t, workerSession)
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", workerSession,
		"Поручение 260031f8 закончено: миграции применены.",
		DeliverOptions{Stage: StageReported, Kind: KindSession, WantSession: workerSession})

	if !d.OK {
		t.Fatalf("доставлено, получено %+v", d)
	}
	select {
	case raw := <-inbox[workerSession]:
		if !strings.Contains(raw, "миграции применены") {
			t.Fatalf("текст уехал целиком, получено %q", raw)
		}
		if !strings.Contains(raw, `"type":"user"`) {
			t.Fatalf("кадр тот, что принимает Claude Code, получено %q", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("разговор получил отчёт")
	}
}

func TestVanishedConversationIsRefusedAndKept(t *testing.T) {
	// Сессия умирает между заведением поручения и отчётом. Молчаливый успех
	// здесь хуже отказа: отчёт считался бы доставленным и выпал бы из дожима.
	claudeHome(t) // реестр пуст
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession,
			WantSession: workerSession, HumanTold: true})

	if d.OK {
		t.Fatal("пропавший разговор — отказ, а не успех")
	}
	if d.Cause != CauseSessionGone {
		t.Fatalf("причина названа, получено %q (%s)", d.Cause, d.Reason)
	}
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatalf("отчёт ждёт в канале вытягивания, получено %+v", recs)
	}
}

func TestSessionReportIsDedupedLikeAnyOther(t *testing.T) {
	// Повторный done не должен будить адресата второй раз: дедупликация по
	// паре задача+стадия общая для всех трёх видов адреса.
	inbox := claudeHome(t, workerSession)
	j := sessionJournal(t)
	o := DeliverOptions{Stage: StageReported, Kind: KindSession, WantSession: workerSession}

	Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт", o)
	recs, _ := j.Read()
	if len(LostReports(recs)) != 0 {
		t.Fatal("доставленный не висит в канале вытягивания")
	}
	<-inbox[workerSession]
	select {
	case raw := <-inbox[workerSession]:
		t.Fatalf("второго сообщения не было, получено %q", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestAddressedConversationIsTheOneRecorded(t *testing.T) {
	// Рядом живёт другой разговор, и промахнуться в него нельзя: отчёт
	// уехал бы тому, кто поручения не давал.
	const neighbour = "aaaaaaaa-0000-0000-0000-000000000000"
	inbox := claudeHome(t, workerSession, neighbour)
	j := sessionJournal(t)

	Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession, WantSession: workerSession})

	select {
	case <-inbox[workerSession]:
	case <-time.After(3 * time.Second):
		t.Fatal("свой разговор получил отчёт")
	}
	select {
	case raw := <-inbox[neighbour]:
		t.Fatalf("соседу не писали, получено %q", raw)
	case <-time.After(300 * time.Millisecond):
	}
}

// abandonedSocket оставляет файл сокета без слушателя: connect в него даёт
// отказ. Иначе этот случай не поставить — Go снимает файл вместе со
// слушателем, и «сокет есть, а принять некому» не воспроизводится.
func abandonedSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "9.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	return p
}

func TestSendFailureIsNotReportedAsDelivery(t *testing.T) {
	// Разговор жив по реестру, а сокет уже никого не слушает: сессия успела
	// кончиться. Объявить это доставкой значит потерять отчёт — он выпал бы
	// из дожима навсегда.
	cfg := t.TempDir()
	home := filepath.Join(cfg, "sessions")
	os.MkdirAll(home, 0o700)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	rec, _ := json.Marshal(map[string]any{
		"pid": 9, "sessionId": workerSession, "name": "worker", "status": "idle",
		"messagingSocketPath": abandonedSocket(t),
	})
	os.WriteFile(filepath.Join(home, "9.json"), rec, 0o600)
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", workerSession, "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession,
			WantSession: workerSession, HumanTold: true})

	if d.OK {
		t.Fatal("сокет не принял — это отказ, а не успех")
	}
	if d.Cause != CauseSendFailed {
		t.Fatalf("причина названа, получено %q (%s)", d.Cause, d.Reason)
	}
	recs, _ := j.Read()
	if len(LostReports(recs)) != 1 {
		t.Fatal("отчёт ждёт в канале вытягивания")
	}
}

func TestPrefixMatchingSeveralConversationsIsItsOwnRefusal(t *testing.T) {
	// «Разговор пропал» и «под начало идентификатора подходит несколько» —
	// разные беды: первую лечит дожим, вторую только человек.
	const a = "5d2958e0-18e2-4193-afa1-2da28def3b14"
	const b = "5d2958e0-99e2-4193-afa1-2da28def3b14"
	claudeHome(t, a, b)
	j := sessionJournal(t)

	d := Deliver(context.Background(), nil, j, "260031f8", "5d2958e0", "отчёт",
		DeliverOptions{Stage: StageReported, Kind: KindSession, HumanTold: true})

	if d.Cause != CauseSessionAmbiguous {
		t.Fatalf("неоднозначность названа своей причиной, получено %q (%s)", d.Cause, d.Reason)
	}
	if !strings.Contains(d.Reason, "18e2") || !strings.Contains(d.Reason, "99e2") {
		t.Fatalf("в отказе названы оба, получено %q", d.Reason)
	}
}

func TestDigestIsAppliedToWhatGoesIntoTheConversation(t *testing.T) {
	// Сводка плюс дайджест работы — то же, что уезжает в тред Codex. Слать в
	// разговор голую сводку значит отдать меньше, чем собрано.
	inbox := claudeHome(t, workerSession)
	j := sessionJournal(t)

	Deliver(context.Background(), nil, j, "260031f8", workerSession, "готово",
		DeliverOptions{Stage: StageReported, Kind: KindSession,
			Compose: func(s string) string { return s + "\n\nЧто делалось: правка конфигурации" }})

	select {
	case raw := <-inbox[workerSession]:
		if !strings.Contains(raw, "правка конфигурации") {
			t.Fatalf("дайджест уехал вместе со сводкой, получено %q", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("разговор получил отчёт")
	}
}

func TestOwnConversationOutranksThePane(t *testing.T) {
	// claudex, запущенный внутри Claude Code, должен возвращать отчёт в свой
	// разговор, а не в панель, которая его держит: за панелью со временем
	// встаёт другой разговор, за идентификатором — всегда тот же.
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	t.Setenv("HERDR_PANE_ID", "wE:p13")

	w := ResolveWake(nil, "", "", "")
	if w.Kind != KindSession || w.Target != workerSession {
		t.Fatalf("адрес — свой разговор, получено %+v", w)
	}
}

func TestNamedPaneStillWinsOverOwnConversation(t *testing.T) {
	// Названный адрес всегда сильнее угаданного: если человек сказал панель,
	// значит панель.
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	if w := ResolveWake(nil, "wE:p2", "", ""); w.Kind != KindPane || w.Target != "wE:p2" {
		t.Fatalf("названная панель выигрывает, получено %+v", w)
	}
}

func TestConversationReportsUnderItsOwnAssignment(t *testing.T) {
	// Поручение в разговор панели не имеет вовсе, и сопоставление по панели
	// отказывало ему в собственном отчёте. Поймано живой проверкой на
	// поручении e7c83a2b: «заведено для другой панели», хотя заведено оно было
	// не для панели.
	recs := []journal.Record{
		{Task: "e7c83a2b", Event: journal.Started, PaneSession: workerSession,
			Target: leaderThread, TargetKind: KindThread, Time: time.Now()},
	}
	r, err := Resolve(recs, workerSession, "")
	if err != nil {
		t.Fatalf("разговор отчитывается по своему поручению, получено %v", err)
	}
	if r.Task != "e7c83a2b" {
		t.Fatalf("поручение опознано, получено %+v", r)
	}
}

func TestPaneAssignmentIsStillMatchedByPane(t *testing.T) {
	// Расширение не должно ломать панельную ветку: у неё исполнитель прежний.
	recs := []journal.Record{
		{Task: "aaaaaaaa", Event: journal.Started, Pane: "wE:p13",
			PaneSession: workerSession, Time: time.Now()},
	}
	if a := ActiveFor(recs, "wE:p13"); len(a) != 1 {
		t.Fatalf("панель находит своё поручение, получено %+v", a)
	}
	if a := ActiveFor(recs, "wE:p99"); len(a) != 0 {
		t.Fatalf("чужая панель — ничего, получено %+v", a)
	}
}

func TestConversationIsPreferredOnlyWhenItHasWork(t *testing.T) {
	// claudex живёт и в панели herdr, и в сессии Claude Code разом. Выбирать
	// исполнителя по тому, какая переменная нашлась первой, нельзя: поручение
	// заведено на что-то одно, и решает журнал.
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	t.Setenv("HERDR_PANE_ID", "wE:p13")

	paneOnly := []journal.Record{
		{Task: "aaaaaaaa", Event: journal.Started, Pane: "wE:p13", Time: time.Now()},
	}
	if got := executorOf(paneOnly, ""); got != "wE:p13" {
		t.Fatalf("работы у разговора нет — отчитываемся панелью, получено %q", got)
	}
	sessionWork := append(paneOnly, journal.Record{
		Task: "bbbbbbbb", Event: journal.Started, PaneSession: workerSession, Time: time.Now()})
	if got := executorOf(sessionWork, ""); got != workerSession {
		t.Fatalf("у разговора есть своё поручение — им и отчитываемся, получено %q", got)
	}
}

// Поручение отправлено без ожидания: наблюдателя за ним нет, и отчёт обязан
// уехать в момент вызова done. Пока в журнале нет записи о завершении отправки,
// Report считает, что доставит наблюдатель, и пропускает доставку — отчёт тогда
// записан, но не ушёл и даже в undelivered не виден. Так и случилось на
// поручении 28f2b0ba: «наблюдатель ещё ждёт — он и доставит», при том что
// ждать было некому.
func TestReportOfANoWaitAssignmentIsDeliveredNotDeferred(t *testing.T) {
	inbox := claudeHome(t, workerSession)
	j := sessionJournal(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)

	j.Append(journal.Record{
		Task: "28f2b0ba", Event: journal.Started, PaneSession: workerSession,
		Target: workerSession, TargetSession: workerSession, TargetKind: KindSession,
		Prompt: "задача", Time: time.Now(),
	})
	if err := SentWithoutWaiting(j, "28f2b0ba", ""); err != nil {
		t.Fatal(err)
	}

	res, err := Report(context.Background(), "28f2b0ba", "готово", "сделано",
		ReportOptions{Journal: j, Deadline: 3 * time.Second, Poll: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != "" {
		t.Fatalf("доставка не откладывается: наблюдателя нет, получено %q", res.Skipped)
	}
	select {
	case raw := <-inbox[workerSession]:
		if !strings.Contains(raw, "сделано") {
			t.Fatalf("отчёт уехал в разговор, получено %q", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("отчёт дошёл до адресата")
	}
}

func TestWithoutTheSentMarkTheReportWaitsForAWatcher(t *testing.T) {
	// Обратная сторона: у ожидающей ветки наблюдатель есть, и вторая доставка
	// из done была бы дублем.
	claudeHome(t, workerSession)
	j := sessionJournal(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)

	j.Append(journal.Record{
		Task: "28f2b0ba", Event: journal.Started, PaneSession: workerSession,
		Target: workerSession, TargetSession: workerSession, TargetKind: KindSession,
		Time: time.Now(),
	})
	res, err := Report(context.Background(), "28f2b0ba", "готово", "сделано",
		ReportOptions{Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped == "" {
		t.Fatal("пока наблюдатель ждёт, done не доставляет сам")
	}
}

func TestRepeatDoneStaysWithTheConversationThatOwnsTheTask(t *testing.T) {
	// Повтор done служит повтором доставки, которая никого не достигла.
	// Закрытое поручение активным не числится, и выбор исполнителя по одной
	// активности откатывался к панели — а у поручения в разговор панели нет,
	// и оно переставало находиться вовсе: «заведено для другой панели», хотя
	// заведено оно было не для панели. Поймано повтором отчёта по 76239557.
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	t.Setenv("HERDR_PANE_ID", "wE:p1A")

	recs := []journal.Record{
		{Task: "76239557", Event: journal.Started, PaneSession: workerSession, Time: time.Now()},
		{Task: "76239557", Event: journal.Reported, Outcome: "готово", Time: time.Now()},
		// У панели своя работа, и её много: прежде выбор уходил к ней.
		{Task: "ad2edb8b", Event: journal.Started, Pane: "wE:p1A", Time: time.Now()},
	}
	if got := executorOf(recs, "76239557"); got != workerSession {
		t.Fatalf("повтор остаётся при своём разговоре, получено %q", got)
	}
	// Без названного поручения прежнее поведение сохраняется.
	if got := executorOf(recs, ""); got != "wE:p1A" {
		t.Fatalf("без названного поручения выбираем панель, получено %q", got)
	}
}

func TestRepeatedReportOnOwnConversationTaskIsAccepted(t *testing.T) {
	// Точный идентификатор собственного поручения — не догадка, даже если
	// отчёт по нему уже был: повтор done служит повтором доставки, которая
	// никого не достигла. Проверка «заводилось ли здесь» смотрела только на
	// панель, и для поручения в разговор давала ложное «заведено другому».
	recs := []journal.Record{
		{Task: "76239557", Event: journal.Started, PaneSession: workerSession, Time: time.Now()},
		{Task: "76239557", Event: journal.Reported, Outcome: "готово", Time: time.Now()},
	}
	r, err := Resolve(recs, workerSession, "76239557")
	if err != nil {
		t.Fatalf("повтор по своему поручению принят, получено %v", err)
	}
	if r.Task != "76239557" || r.Corrected {
		t.Fatalf("остаёмся на названном, получено %+v", r)
	}
}

func TestRefusalNamesAConversationAsAConversation(t *testing.T) {
	// Отказ читает человек, который разбирается, что пошло не так. Назвать
	// разговор панелью значит отправить его искать несуществующую панель.
	recs := []journal.Record{
		{Task: "aaaaaaaa", Event: journal.Started, PaneSession: workerSession, Time: time.Now()},
		{Task: "bbbbbbbb", Event: journal.Started, PaneSession: workerSession, Time: time.Now()},
	}
	_, err := Resolve(recs, workerSession, "")
	if err == nil {
		t.Fatal("двусмысленность отклонена")
	}
	if strings.Contains(err.Error(), "панели") {
		t.Fatalf("разговор не назван панелью, получено %q", err)
	}
	if !strings.Contains(err.Error(), "разговора 44d5f44b") {
		t.Fatalf("разговор назван разговором, получено %q", err)
	}
}

const parentPane = "wE:p1A"

// mixedJournal воспроизводит состояние, в котором оказался живой журнал:
// родительское поручение заведено панели и ещё не отчиталось, а рядом лежит
// закрытый внутренний smoke, исполнителем которого был разговор.
func mixedJournal() []journal.Record {
	now := time.Now()
	return []journal.Record{
		{Task: "d21bb044", Event: journal.Started, Pane: parentPane,
			Target: "01a0a268-ca14-7441-844b-8fcd24cd2e45", TargetKind: KindThread, Time: now},
		{Task: "d21bb044", Event: journal.Finished, Pane: parentPane, Outcome: SentNoWait, Time: now},
		{Task: "76239557", Event: journal.Started, PaneSession: workerSession,
			Target: workerSession, TargetKind: KindSession, Time: now},
		{Task: "76239557", Event: journal.Finished, Outcome: SentNoWait, Time: now},
		{Task: "76239557", Event: journal.Reported, Outcome: "готово", Time: now},
	}
}

func TestParentPaneAssignmentStillReportsUnderItself(t *testing.T) {
	// Родитель заведён панели, внутренний smoke — разговору. Выбор исполнителя
	// не должен уводить отчёт родителя к разговору: у панели поручение живо, у
	// разговора закрыто.
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	t.Setenv("HERDR_PANE_ID", parentPane)
	recs := mixedJournal()

	if got := executorOf(recs, "d21bb044"); got != parentPane {
		t.Fatalf("родитель отчитывается своей панелью, получено %q", got)
	}
	r, err := Resolve(recs, executorOf(recs, "d21bb044"), "d21bb044")
	if err != nil {
		t.Fatalf("родитель сопоставлен, получено %v", err)
	}
	if r.Task != "d21bb044" || r.Corrected {
		t.Fatalf("остаёмся на родителе без подмены, получено %+v", r)
	}
}

func TestClosedSmokeNeverSubstitutesForTheParent(t *testing.T) {
	// Прямая подмена: отчёт родителя не должен записаться под внутренним
	// поручением, даже когда идентификатор не назван.
	t.Setenv("CLAUDE_CODE_SESSION_ID", workerSession)
	t.Setenv("HERDR_PANE_ID", parentPane)
	recs := mixedJournal()

	// Без названного идентификатора отчёт обязан лечь на родителя: у панели
	// он единственный живой. Уехать под закрытый внутренний smoke он не может
	// ни при каких обстоятельствах.
	r, err := Resolve(recs, executorOf(recs, ""), "")
	if err != nil {
		t.Fatalf("родитель у панели один — отчёт ложится на него, получено %v", err)
	}
	if r.Task != "d21bb044" {
		t.Fatalf("отчёт ушёл не родителю, получено %+v", r)
	}
	if r.Task == "76239557" {
		t.Fatal("внутренний smoke родителя не подменяет")
	}
	for _, a := range ActiveFor(recs, workerSession) {
		if a.Task == "d21bb044" {
			t.Fatal("родитель не числится за разговором")
		}
	}
	if a := ActiveFor(recs, parentPane); len(a) != 1 || a[0].Task != "d21bb044" {
		t.Fatalf("у панели активен ровно родитель, получено %+v", a)
	}
}

func TestSilentConversationTaskIsSurfacedNotLost(t *testing.T) {
	// Сессия довела работу до конца и не вызвала done. Экрана у разговора нет,
	// сочинять отчёт нельзя — но и пропадать поручение не должно: сегодня
	// silent() отсеивал всё беспанельное, и такое поручение не попадало ни в
	// сборку, ни в канал вытягивания.
	claudeHome(t, workerSession) // разговор жив и простаивает
	recs := []journal.Record{
		{Task: "5a023d9b", Event: journal.Started, PaneSession: workerSession,
			Target: workerSession, TargetSession: workerSession, TargetKind: KindSession,
			Time: time.Now().Add(-2 * time.Hour)},
		{Task: "5a023d9b", Event: journal.Finished, Outcome: SentNoWait,
			Time: time.Now().Add(-2 * time.Hour)},
	}
	j := sessionJournal(t)
	for _, r := range recs {
		j.Append(r)
	}

	got := Reconcile(context.Background(), ReconcileOptions{
		Journal: j, MinAge: time.Minute, Now: time.Now(),
	})
	if len(got) != 1 {
		t.Fatalf("молчаливое поручение разговора видно сборке, получено %+v", got)
	}
	if got[0].Synthetic {
		t.Fatal("текста нет — сочинять отчёт нельзя")
	}
	if got[0].Cause == "" {
		t.Fatalf("причина названа, получено %+v", got[0])
	}
}

func TestBusyConversationIsNotCalledSilent(t *testing.T) {
	// Занятость — не молчание. Назвать работающий разговор молчаливым значит
	// подтолкнуть ведущего дёргать того, кто ещё работает.
	cfg := t.TempDir()
	home := filepath.Join(cfg, "sessions")
	os.MkdirAll(home, 0o700)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sock, _ := os.MkdirTemp("/tmp", "cx")
	t.Cleanup(func() { os.RemoveAll(sock) })
	p := filepath.Join(sock, "7.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	rec, _ := json.Marshal(map[string]any{
		"pid": 7, "sessionId": workerSession, "name": "worker",
		"status": "busy", "messagingSocketPath": p,
	})
	os.WriteFile(filepath.Join(home, "7.json"), rec, 0o600)

	j := sessionJournal(t)
	j.Append(journal.Record{Task: "5a023d9b", Event: journal.Started,
		PaneSession: workerSession, Target: workerSession, TargetKind: KindSession,
		Time: time.Now().Add(-2 * time.Hour)})

	got := Reconcile(context.Background(), ReconcileOptions{
		Journal: j, MinAge: time.Minute, Now: time.Now()})
	if len(got) != 1 || got[0].Cause != CauseExecutorBusy {
		t.Fatalf("работающий разговор не объявлен молчаливым, получено %+v", got)
	}
}

func TestSilentConversationReportIsNeverInvented(t *testing.T) {
	// Тот же запрет, что и на пустом экране панели: отсутствие доказательства
	// не повод записать отчёт.
	claudeHome(t, workerSession)
	j := sessionJournal(t)
	j.Append(journal.Record{Task: "5a023d9b", Event: journal.Started,
		PaneSession: workerSession, Target: workerSession, TargetKind: KindSession,
		Time: time.Now().Add(-2 * time.Hour)})

	got := Reconcile(context.Background(), ReconcileOptions{
		Journal: j, MinAge: time.Minute, Now: time.Now()})
	if len(got) != 1 || got[0].Cause != CauseNoScreen {
		t.Fatalf("причина названа, получено %+v", got)
	}
	recs, _ := j.Read()
	for _, r := range recs {
		if r.Event == journal.Reported {
			t.Fatal("ложного отчёта в журнале нет")
		}
	}
}
