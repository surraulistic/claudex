package main

import (
	"strings"
	"testing"

	"github.com/surraulistic/claudex/internal/exitcode"
	"github.com/surraulistic/claudex/internal/task"
)

func TestSendDoesNotOccupyTheTurnByDefault(t *testing.T) {
	// Главное отличие send от delegate. Ожидание стоит хода, а ждать приходится
	// редко — поэтому оно стало явным.
	var o opts
	if o.wait {
		t.Fatal("умолчание — не ждать")
	}
	o.noWait = !o.wait
	if !o.noWait {
		t.Fatal("send по умолчанию отправляет и выходит")
	}
	o.wait = true
	o.noWait = !o.wait
	if o.noWait {
		t.Fatal("--wait возвращает ожидание")
	}
}

func TestHeadlessRefusesLoudlyAndSaysWhy(t *testing.T) {
	// Отказ намеренно многословен: рабочие headless уже запускают мимо
	// claudex, и такая работа не оставляет следа. Молчаливый отказ этого не
	// объяснит.
	err := headlessNotReady("цель", "текст")
	if err == nil {
		t.Fatal("пока отказ")
	}
	for _, want := range []string{"claude --bg", "журнал", "send"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("в отказе сказано про %q, получено %q", want, err)
		}
	}
	if exitcode.Of(err) != exitcode.BadCall {
		t.Fatalf("код вызова, получено %d", exitcode.Of(err))
	}
}

func TestTaskSubcommandNeverGuessesWhatItWasGiven(t *testing.T) {
	// «task» принимает и подкоманду, и идентификатор. Спутать их нельзя:
	// опечатка не должна молча показать чужое поручение.
	err := cmdTask(opts{}, []string{"листь"})
	if err == nil || !strings.Contains(err.Error(), "не похоже на идентификатор") {
		t.Fatalf("опечатка названа опечаткой, получено %v", err)
	}
	if err := cmdTask(opts{}, []string{"digest"}); err == nil ||
		!strings.Contains(err.Error(), "нужен идентификатор") {
		t.Fatalf("digest без идентификатора отклонён, получено %v", err)
	}
}

func TestNewWorkerFlagIsReservedNotSilentlyIgnored(t *testing.T) {
	// Нереализованный флаг, который молча ничего не делает, хуже отсутствующего.
	err := cmdSend(opts{newSession: true}, "цель", "текст")
	if err == nil || !strings.Contains(err.Error(), "--new") {
		t.Fatalf("зарезервированный флаг отказывает явно, получено %v", err)
	}
}

func TestPublicSurfaceIsDocumented(t *testing.T) {
	// Команда, которой нет в справке, не существует для человека.
	for _, c := range []string{
		"claudex send", "claudex task list", "claudex task <id>",
		"claudex task log", "claudex doctor",
	} {
		if !strings.Contains(helpText, c) {
			t.Errorf("в справке нет %q", c)
		}
	}
	// Прежние имена обещаны совместимыми — обещание должно быть записано.
	if !strings.Contains(helpText, "delegate = send") {
		t.Error("в справке не сказано, что прежние имена работают")
	}
	// И сказано, чем tell отличается от send.
	if !strings.Contains(helpText, "это send, а не tell") {
		t.Error("в справке не сказано, когда tell не годится")
	}
}

func TestLifecycleStatesAreNamedInOnePlace(t *testing.T) {
	// Состояния — часть договора с наблюдателем, который появится следующим.
	for _, s := range []string{
		task.StateCreated, task.StateSent, task.StateWorking, task.StateProgress,
		task.StateNeedsInput, task.StateDone, task.StateFailed,
		task.StateUndelivered, task.StateLost,
	} {
		if s == "" {
			t.Fatal("состояние без имени")
		}
	}
}

func TestSupervisorSubcommandDoesNotGuess(t *testing.T) {
	err := cmdSupervisor(opts{}, []string{"старт"})
	if err == nil || !strings.Contains(err.Error(), "не подкоманда") {
		t.Fatalf("опечатка названа опечаткой, получено %v", err)
	}
}

func TestSupervisorIsDocumentedAsNonAuthoring(t *testing.T) {
	// Главное свойство наблюдателя — он ничего не сочиняет. Если справка об
	// этом молчит, его примут за бота, пишущего в диалог вместо координатора.
	if !strings.Contains(helpText, "claudex supervisor") {
		t.Fatal("наблюдателя нет в справке")
	}
	for _, want := range []string{"ничего не\n                                         сочиняет", "status", "stop"} {
		if !strings.Contains(helpText, want) {
			t.Errorf("в справке не сказано про %q", want)
		}
	}
}

func TestRepairCommandsAreMarkedAsManual(t *testing.T) {
	// flush и reconcile остаются, но обиходом не являются: их теперь делает
	// наблюдатель сам.
	if !strings.Contains(helpText, "починка вручную") {
		t.Error("flush/reconcile не помечены как ручная починка")
	}
}

func TestSessionListIsTheRecommendedDiscovery(t *testing.T) {
	// «Кому можно дать работу» и «что я уже поручил» — разные вопросы, и
	// путать их дорого.
	if !strings.Contains(helpText, "claudex session list") {
		t.Fatal("session list нет в справке")
	}
	if !strings.Contains(helpText, "claudex peers") {
		t.Error("прежнее имя не названо совместимым")
	}
	// Инвариант: рабочий адрес — идентификатор, а не имя панели.
	if !strings.Contains(helpText, "идентификатор разговора →") {
		t.Error("в справке не описано распознавание адресата")
	}
}

func TestEventWakeIsDocumentedAsTheNormalPath(t *testing.T) {
	// Если справка молчит, «почему мне прилетела сводка» останется без ответа.
	if !strings.Contains(helpText, "--full-report") {
		t.Fatal("флага возврата к прежнему поведению нет в справке")
	}
	if !strings.Contains(helpText, "событием") {
		t.Error("в справке не сказано, что обычный путь — событие")
	}
}
