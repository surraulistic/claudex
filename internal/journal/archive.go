package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Архивация журнала.
//
// Журнал дописывается и не сжимается: за две с половиной недели два мегабайта,
// полторы тысячи записей, 456 поручений. Читается он при каждом вызове, и чем
// дальше, тем дороже.
//
// Старое не удаляется, а переезжает: разбор инцидента полугодовой давности —
// обычное дело, и потерять записи ради скорости значит поменять одно на другое
// без спроса. Архив лежит рядом и читается тем же разбором.

// ArchiveResult — что переехало.
type ArchiveResult struct {
	Path     string `json:"path"`
	Moved    int    `json:"moved"`
	Kept     int    `json:"kept"`
	Tasks    int    `json:"tasks"`
	DryRun   bool   `json:"dry_run"`
	Skipped  int    `json:"skipped_open"`
	SinceDay string `json:"older_than"`
}

// Archive уносит в отдельный файл записи поручений, по которым давно ничего не
// происходит.
//
// Переезжает поручение целиком или не переезжает вовсе: половина записей в
// журнале, половина в архиве — это разорванная история, по которой уже не
// разобрать, что было.
func Archive(path string, older time.Duration, dry bool) (ArchiveResult, error) {
	res := ArchiveResult{DryRun: dry, SinceDay: time.Now().Add(-older).Format("2006-01-02")}
	j := Open(path)
	recs, err := j.Read()
	if err != nil {
		return res, err
	}
	if len(recs) == 0 {
		return res, nil
	}

	cut := time.Now().Add(-older)
	last := map[string]time.Time{}
	for _, r := range recs {
		if r.Time.After(last[r.Task]) {
			last[r.Task] = r.Time
		}
	}
	old := map[string]bool{}
	for id, t := range last {
		if t.Before(cut) {
			old[id] = true
		}
	}

	var keep, move []Record
	for _, r := range recs {
		if old[r.Task] {
			move = append(move, r)
		} else {
			keep = append(keep, r)
		}
	}
	res.Moved, res.Kept, res.Tasks = len(move), len(keep), len(old)
	if dry || len(move) == 0 {
		return res, nil
	}

	res.Path = filepath.Join(filepath.Dir(path),
		fmt.Sprintf("tasks-archive-%s.jsonl", time.Now().Format("2006-01-02")))
	// Сначала дописываем архив, и только потом урезаем журнал. Обратный
	// порядок теряет записи на первом же сбое между двумя действиями.
	if err := appendAll(res.Path, move); err != nil {
		return res, err
	}
	if err := rewrite(path, keep); err != nil {
		return res, err
	}
	return res, nil
}

func writeLine(f *os.File, r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func appendAll(path string, recs []Record) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, r := range recs {
		if err := writeLine(f, r); err != nil {
			return err
		}
	}
	return f.Sync()
}

// rewrite меняет журнал через временный файл: оборванная запись на месте
// живого журнала хуже, чем неархивированный журнал.
func rewrite(path string, recs []Record) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := writeLine(f, r); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}
