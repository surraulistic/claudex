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
	// Idle — исполнитель закончил ход и ничего больше не делает. Пишется
	// хуком Stop самого Claude Code: платформа и так сообщает об окончании
	// хода и отдаёт текст последней реплики, и уговаривать задачу помнить про
	// `claudex done` ради того же незачем.
	//
	// Завершением поручения сама по себе не является: ход кончается и посреди
	// работы. Решает наблюдатель — по тому, сколько исполнитель молчит.
	Idle = "idle"
	// Refused — поручение не было отправлено вовсе. Пишется вместе с текстом:
	// иначе он исчезает совсем, а вызывающему доложено об успехе.
	Refused = "refused"
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
	Stage string `json:"stage,omitempty"`
	// Cause — машиночитаемая причина, по которой доставка не состоялась.
	Cause string `json:"cause,omitempty"`
	// Delivery — чем будили: минимальным событием или отчётом целиком. Без
	// этого по записи не отличить одно от другого, а разбираться потом.
	Delivery string `json:"delivery,omitempty"`
	// TargetSession — разговор, который был в целевой панели, когда поручение
	// заводили. Панель переживает смену агента, разговор — нет, и будить надо
	// именно тот, что поручение затеял.
	TargetSession string `json:"target_session,omitempty"`
	// TargetKind — чем является Target: панелью herdr или тредом Codex.
	// Пусто читается как панель: записи, заведённые до появления адресации по
	// треду, других адресов не знали.
	TargetKind string `json:"target_kind,omitempty"`
	// ClaimedTask — идентификатор, который назвал отчитывающийся, если он
	// отличается от того, под которым запись легла. Пусто значит «назвали
	// верно». Держать его обязательно: исправление должно быть видно в журнале,
	// иначе разбор инцидента опирается на догадку.
	ClaimedTask string `json:"claimed_task,omitempty"`
	// Correction — почему идентификатор пришлось исправить.
	Correction string `json:"correction,omitempty"`
	// TargetState и TargetSeen — доказательство привязки: чем был адрес в
	// момент заведения поручения и когда это проверяли. Без них «тред был жив»
	// остаётся словами: к моменту отчёта разговор успевает закрыться, и по
	// журналу не отличить недосмотр от честной смены состояния.
	// PaneSession — разговор, который шёл в панели-исполнителе в момент
	// заведения. Без него нельзя доказать, что закончил именно он: панель
	// переживает смену агента, и «панель освободилась» доказательством не
	// является.
	PaneSession string `json:"pane_session,omitempty"`
	// Synthetic — отчёт собран ClauDex из живого экрана панели, а не назван
	// самой задачей. Помечается, потому что доверие к нему другое.
	Synthetic   bool      `json:"synthetic,omitempty"`
	TargetState string    `json:"target_state,omitempty"`
	TargetSeen  time.Time `json:"target_seen,omitempty"`
	Attempt     int       `json:"attempt,omitempty"`
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
