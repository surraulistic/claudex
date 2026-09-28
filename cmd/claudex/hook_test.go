package main

import (
	"errors"
	"testing"
	"time"

	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
)

const hookSession = "7e403273-2034-4296-a833-d176bd30e03d"

func started(id string) journal.Record {
	return journal.Record{Task: id, Event: journal.Started,
		PaneSession: hookSession, Time: time.Now().Add(-time.Hour)}
}

func TestIdleMarkIsSkippedWhenSeveralTasksAreOpen(t *testing.T) {
	// Конец хода не говорит, какое из поручений закончилось. Пометить все
	// значит записать догадку: на живом журнале у одного исполнителя их было
	// 62, и один конец хода разбудил бы затеявшего по каждому.
	recs := []journal.Record{started("aaaaaaaa"), started("bbbbbbbb")}
	if id, ok := idleMark(recs, hookSession, "текст"); ok {
		t.Fatalf("при нескольких открытых не метим, получено %q", id)
	}
}

func TestIdleMarkIsWrittenForTheOnlyOpenTask(t *testing.T) {
	recs := []journal.Record{started("aaaaaaaa")}
	id, ok := idleMark(recs, hookSession, "миграции применены")
	if !ok || id != "aaaaaaaa" {
		t.Fatalf("единственное открытое отмечается, получено %q %v", id, ok)
	}
}

func TestNothingOpenMeansNothingToMark(t *testing.T) {
	if _, ok := idleMark(nil, hookSession, "текст"); ok {
		t.Fatal("без поручений журнал не трогаем — иначе это лог ходов")
	}
}

func TestRepeatedIdenticalMarkIsNotWrittenTwice(t *testing.T) {
	// Ход кончается много раз подряд; повтор того же текста журналу ничего не
	// добавляет — за час их набралось семь по одному поручению.
	recs := []journal.Record{
		started("aaaaaaaa"),
		{Task: "aaaaaaaa", Event: journal.Idle, Reason: "тот же текст"},
	}
	if _, ok := idleMark(recs, hookSession, "тот же текст"); ok {
		t.Fatal("повтор не пишется")
	}
	if id, ok := idleMark(recs, hookSession, "новый текст"); !ok || id != "aaaaaaaa" {
		t.Fatalf("изменившийся текст пишется, получено %q %v", id, ok)
	}
}

func TestSendContinuesTheOnlyOpenAssignment(t *testing.T) {
	// Отправка работнику, у которого работа идёт, — продолжение, а не второе
	// поручение: прежде каждая отправка заводила отдельное, и у одного
	// исполнителя их накопилось 62.
	recs := []journal.Record{started("aaaaaaaa")}
	id, err := continueOrStart(recs, hookSession, false)
	if err != nil || id != "aaaaaaaa" {
		t.Fatalf("продолжаем начатое, получено %q %v", id, err)
	}
}

func TestSendStartsFreshWhenNothingIsOpen(t *testing.T) {
	id, err := continueOrStart(nil, hookSession, false)
	if err != nil || id != "" {
		t.Fatalf("работы нет — заводим новое, получено %q %v", id, err)
	}
}

func TestNewFlagForcesASeparateAssignment(t *testing.T) {
	recs := []journal.Record{started("aaaaaaaa")}
	id, err := continueOrStart(recs, hookSession, true)
	if err != nil || id != "" {
		t.Fatalf("--new заводит отдельное, получено %q %v", id, err)
	}
}

func TestSeveralOpenAssignmentsRefuseRatherThanPick(t *testing.T) {
	recs := []journal.Record{started("aaaaaaaa"), started("bbbbbbbb")}
	_, err := continueOrStart(recs, hookSession, false)
	var many *task.TooManyOpen
	if !errors.As(err, &many) {
		t.Fatalf("какое продолжают — знает только человек, получено %v", err)
	}
	if len(many.Open) != 2 {
		t.Fatalf("названы оба, получено %+v", many.Open)
	}
}
