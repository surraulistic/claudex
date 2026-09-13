// Package journal — дописываемый журнал задач. Через него делегированная
// задача отчитывается о завершении: она запускается в чужой панели и чужом
// процессе, поэтому отчёт приходит файлом, а не по памяти.
package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	Started  = "started"
	Reported = "reported"
	Finished = "finished"
	// Notified — чем кончилась попытка разбудить ведущего. Раньше это писалось
	// только в лог отсоединённого наблюдателя, который никто не читает, и
	// недоставленное пробуждение было невидимым.
	Notified = "notified"
)

type Record struct {
	Time    time.Time `json:"time"`
	Task    string    `json:"task"`
	Event   string    `json:"event"`
	Pane    string    `json:"pane,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Prompt  string    `json:"prompt,omitempty"`
	Target  string    `json:"target,omitempty"`
	// Stage — какую стадию доставляла запись notified. Исход по сроку и
	// пришедший позже настоящий отчёт — разные события, и доставляются они
	// порознь.
	Stage   string `json:"stage,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

type Journal struct {
	Path string
	poll time.Duration
}

func Open(path string) *Journal { return &Journal{Path: path, poll: 50 * time.Millisecond} }

// Append дописывает запись одной строкой одним вызовом write: файл открыт на
// дозапись, поэтому параллельные писатели не перемешивают строки.
func (j *Journal) Append(r Record) error {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.Path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(j.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func (j *Journal) Read() ([]Record, error) {
	f, err := os.Open(j.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r Record
		// Обрыв записи на падении портит одну строку; остальные читаются.
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Event != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// Await ждёт запись по задаче. Сначала смотрит уже записанное: задача может
// отчитаться быстрее, чем ожидающий начнёт слушать, и в эту щель
// проваливается всё делегирование.
func (j *Journal) Await(ctx context.Context, task string, events ...string) (Record, error) {
	want := map[string]bool{}
	for _, e := range events {
		want[e] = true
	}
	for {
		recs, err := j.Read()
		if err != nil {
			return Record{}, err
		}
		for _, r := range recs {
			if r.Task == task && want[r.Event] {
				return r, nil
			}
		}
		select {
		case <-ctx.Done():
			return Record{}, fmt.Errorf("отчёт по задаче %s не пришёл: %w", task, ctx.Err())
		case <-time.After(j.poll):
		}
	}
}
