// Package freshness отвечает на один вопрос: не отстала ли проиндексированная
// история от того, что происходило на самом деле.
//
// Индекс собирается из базы cass, а cass читает транскрипты своим ходом
// (`cass index --watch --watch-interval 60`). Наблюдалось отставание в час при
// живом наблюдателе. Молча выдавать такую историю за текущую нельзя: рядом
// лежат живой хвост панели и журнал поручений, и они говорят другое.
package freshness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Два допуска, а не один, и это не придирка.
//
// SourceTolerance закрывает обычный ход опроса: cass ходит по файлам раз в
// минуту и обрабатывает их пачками, наблюдавшееся рядовое отставание — около
// трёх минут. Порог ниже сделал бы признак постоянным шумом.
//
// JournalTolerance куда строже, потому что свидетельство другого рода.
// Завершённое поручение — этоне догадка о содержимом файла, а записанный
// факт: работа на этой панели закончилась в такой-то момент. Если история его
// не содержит, она отстаёт, и ждать трёх минут, чтобы это признать, незачем.
const (
	SourceTolerance  = 10 * time.Minute
	JournalTolerance = time.Minute
)

const (
	CauseSourceBehind = "source_behind_file" // транскрипт дописан позже последней записи
	CauseJournalAhead = "journal_ahead"      // поручение завершилось позже последней записи
)

// Lag — отставание истории, каким его можно доказать. Ноль значений не бывает:
// если возвращён не nil, отставание измерено.
type Lag struct {
	ObservedAt    time.Time `json:"observed_at"`
	BehindSeconds int64     `json:"behind_seconds"`
	SourceNewest  *string   `json:"source_newest,omitempty"`
	Cause         string    `json:"cause"`
	Reason        string    `json:"reason"`
}

// Check сверяет последнюю проиндексированную запись с двумя независимыми
// свидетелями: временем правки самого транскрипта и последним завершённым
// поручением из журнала. Журнал нужен отдельно — у панели Codex транскрипт
// может быть не найден, а поручение всё равно доказывает, что работа шла.
//
// sourcePath берётся из индекса (`conv.source_path`), то есть это тот путь,
// который записал сам cass, а не наша догадка. Пустое время свидетеля значит
// «свидетеля нет».
func Check(lastActivity, sourceNewest, journalNewest, now time.Time) *Lag {
	lag := &Lag{ObservedAt: now}

	if !sourceNewest.IsZero() && sourceNewest.After(lastActivity.Add(SourceTolerance)) {
		s := sourceNewest.Format(time.RFC3339)
		lag.SourceNewest = &s
		lag.BehindSeconds = int64(sourceNewest.Sub(lastActivity).Seconds())
		lag.Cause = CauseSourceBehind
		lag.Reason = fmt.Sprintf(
			"в транскрипте есть запись от %s, а последняя проиндексированная — %s: источник (cass) ещё не прочитал файл",
			sourceNewest.In(time.Local).Format("15:04:05"), lastActivity.Format("15:04:05"))
	}

	if !journalNewest.IsZero() && journalNewest.After(lastActivity.Add(JournalTolerance)) {
		behind := int64(journalNewest.Sub(lastActivity).Seconds())
		if behind > lag.BehindSeconds {
			lag.BehindSeconds = behind
			lag.Cause = CauseJournalAhead
			lag.Reason = fmt.Sprintf(
				"поручение завершилось в %s, а последняя проиндексированная запись — %s: журнал поручений опережает историю",
				journalNewest.Format("15:04:05"), lastActivity.Format("15:04:05"))
		}
	}

	if lag.Cause == "" {
		return nil
	}
	return lag
}

// tailWindow — сколько байт с конца читать в поисках последней записи.
// Записи транскрипта бывают крупными (вложенная выдача инструментов), но
// четверти мегабайта хватает с запасом, а файл при этом может весить сотню.
const tailWindow = 256 << 10

// LastRecord — время последней записи в самом транскрипте.
//
// Время правки файла для этого не годится: измерено, транскрипт трогают без
// новых записей — у одной панели mtime опережал последнюю запись на пятнадцать
// часов, у другой на тридцать четыре минуты. Сравнивать надо содержимое с
// содержимым, иначе признак отставания кричит впустую.
//
// Отсутствие файла не ошибка: закрытая сессия могла быть перенесена, и тогда
// свидетеля просто нет.
func LastRecord(path string, notBefore time.Time) time.Time {
	if path == "" {
		return time.Time{}
	}
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	// Файл не трогали с тех пор — новых записей в нём быть не может, и читать
	// его незачем.
	if !st.ModTime().After(notBefore) {
		return time.Time{}
	}
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}
	}
	defer f.Close()

	size := st.Size()
	from := size - tailWindow
	if from < 0 {
		from = 0
	}
	buf := make([]byte, size-from)
	if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
		return time.Time{}
	}

	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(lines[i], &rec) != nil || rec.Timestamp == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, rec.Timestamp); err == nil {
			return t
		}
	}
	return time.Time{}
}
