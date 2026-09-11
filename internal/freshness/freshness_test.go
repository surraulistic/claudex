package freshness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 11, 16, 52, 36, 0, time.Local)

func TestFreshHistoryIsNotFlagged(t *testing.T) {
	// Пока панель работает, транскрипт дописывается постоянно и всегда чуть
	// свежее последней проиндексированной записи. Кричать на это нельзя.
	for _, gap := range []time.Duration{0, time.Second, 3 * time.Minute, SourceTolerance} {
		if l := Check(base, base.Add(gap), time.Time{}, base); l != nil {
			t.Fatalf("отставание %v — обычный ход дел, получено %+v", gap, l)
		}
	}
}

func TestFileNewerThanIndexIsReported(t *testing.T) {
	// Тот самый случай: поручение завершилось, транскрипт дописан, а история
	// осталась на часовой давности.
	mod := base.Add(time.Hour + 7*time.Minute)
	l := Check(base, mod, time.Time{}, mod.Add(time.Minute))
	if l == nil {
		t.Fatal("час отставания — не обычный ход дел")
	}
	if l.Cause != CauseSourceBehind {
		t.Fatalf("причина названа, получено %q", l.Cause)
	}
	if l.BehindSeconds != 4020 {
		t.Fatalf("отставание измерено, получено %d с", l.BehindSeconds)
	}
	if l.SourceNewest == nil {
		t.Fatal("время правки транскрипта — часть доказательства")
	}
	if l.ObservedAt.IsZero() {
		t.Fatal("момент наблюдения проставлен: без него выдачу нельзя перечитать позже")
	}
}

func TestJournalWitnessIsStricterThanTheFile(t *testing.T) {
	// Обычная задержка опроса у файла — три минуты; шуметь на неё нельзя.
	// Но завершённое поручение это не догадка о содержимом, а записанный факт,
	// и ждать десяти минут, чтобы признать отставание, незачем.
	gap := 3 * time.Minute
	if l := Check(base, base.Add(gap), time.Time{}, base); l != nil {
		t.Fatal("три минуты у файла — обычный ход опроса")
	}
	l := Check(base, time.Time{}, base.Add(gap), base)
	if l == nil || l.Cause != CauseJournalAhead {
		t.Fatalf("три минуты у журнала — уже расхождение, получено %+v", l)
	}
}

func TestJournalWitnessWorksWithoutFile(t *testing.T) {
	// У панели Codex транскрипта может не найтись, но завершённое поручение
	// всё равно доказывает, что работа шла после последней записи.
	done := base.Add(30 * time.Minute)
	l := Check(base, time.Time{}, done, done)
	if l == nil || l.Cause != CauseJournalAhead {
		t.Fatalf("журнал — самостоятельный свидетель, получено %+v", l)
	}
	if l.SourceNewest != nil {
		t.Fatal("без файла времени правки нет, и выдумывать его нельзя")
	}
}

func TestStrongerWitnessWins(t *testing.T) {
	// Оба свидетеля говорят об отставании — берётся тот, что дальше, и
	// называется именно он.
	l := Check(base, base.Add(10*time.Minute), base.Add(time.Hour), base)
	if l.Cause != CauseJournalAhead || l.BehindSeconds != 3600 {
		t.Fatalf("получено %+v", l)
	}
	l = Check(base, base.Add(time.Hour), base.Add(10*time.Minute), base)
	if l.Cause != CauseSourceBehind || l.BehindSeconds != 3600 {
		t.Fatalf("получено %+v", l)
	}
}

func TestMissingFileIsNotAWitness(t *testing.T) {
	if got := LastRecord(filepath.Join(t.TempDir(), "нет.jsonl"), base); !got.IsZero() {
		t.Fatalf("отсутствие файла — не свидетельство, получено %v", got)
	}
	if got := LastRecord("", base); !got.IsZero() {
		t.Fatal("пустой путь тоже")
	}
}

// transcript пишет транскрипт с указанными метками и задаёт файлу время правки.
func transcript(t *testing.T, stamps []string, mod time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "с.jsonl")
	var body []byte
	for _, s := range stamps {
		body = append(body, []byte(`{"type":"assistant","timestamp":"`+s+`"}`+"\n")...)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLastRecordReadsContentNotModTime(t *testing.T) {
	// Измерено на живых панелях: транскрипт трогают без новых записей — у
	// одной mtime опережал последнюю запись на пятнадцать часов. Верить надо
	// содержимому, иначе признак отставания кричит впустую.
	content := base.Add(-2 * time.Hour).UTC()
	p := transcript(t, []string{
		content.Add(-time.Minute).Format(time.RFC3339),
		content.Format(time.RFC3339),
	}, base.Add(15*time.Hour))

	got := LastRecord(p, base.Add(-3*time.Hour))
	if !got.Equal(content) {
		t.Fatalf("взято время последней записи, получено %v, ожидалось %v", got, content)
	}
	if Check(base, got, time.Time{}, base) != nil {
		t.Fatal("запись старше проиндексированной — отставания нет, что бы ни говорил mtime")
	}
}

func TestUntouchedFileIsNotRead(t *testing.T) {
	// Если файл не трогали с момента последней проиндексированной записи,
	// новых записей в нём быть не может — читать сотню мегабайт незачем.
	p := transcript(t, []string{base.UTC().Format(time.RFC3339)}, base.Add(-time.Hour))
	if got := LastRecord(p, base); !got.IsZero() {
		t.Fatalf("нетронутый файл не читается, получено %v", got)
	}
}

func TestLastRecordSkipsTrailingLinesWithoutTimestamp(t *testing.T) {
	// В хвосте файла лежат служебные строки без метки времени; искать надо
	// снизу вверх до первой настоящей.
	want := base.Add(-time.Hour).UTC()
	p := filepath.Join(t.TempDir(), "с.jsonl")
	body := `{"type":"assistant","timestamp":"` + want.Format(time.RFC3339) + `"}` + "\n" +
		`{"type":"summary"}` + "\n" + `не json` + "\n" + "\n"
	os.WriteFile(p, []byte(body), 0o644)
	os.Chtimes(p, base.Add(time.Hour), base.Add(time.Hour))
	if got := LastRecord(p, base.Add(-2*time.Hour)); !got.Equal(want) {
		t.Fatalf("получено %v, ожидалось %v", got, want)
	}
}
