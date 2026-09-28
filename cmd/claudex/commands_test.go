package main

import (
	"flag"
	"strings"
	"testing"
)

// Списков было два — разбор и справка, — и они расходились дважды:
// --notify-session не попал в справку вовсе, session list и --panel оказались
// только в примерах. Пока согласие держится на внимательности, оно теряется.

func TestEveryPublicCommandIsInTheHelp(t *testing.T) {
	for _, c := range commands() {
		if c.Internal || c.Legacy {
			// Прежние имена названы отдельной строкой про совместимость —
			// у них своя проверка ниже.
			continue
		}
		if !strings.Contains(helpText, "claudex "+c.Name) {
			t.Errorf("команда %q не описана в справке", c.Name)
		}
	}
}

func TestInternalCommandsAreSaidToBeInternal(t *testing.T) {
	// Служебное тоже должно быть названо — но отдельно, чтобы его не приняли
	// за обиход. Молчать о нём нельзя: человек всё равно наткнётся.
	i := strings.Index(helpText, "Не для обихода:")
	if i < 0 {
		t.Fatal("раздела служебных команд в справке нет")
	}
	tail := helpText[i:]
	for _, c := range commands() {
		if !c.Internal || c.Name == "hook" {
			continue
		}
		if !strings.Contains(tail, "claudex "+c.Name) {
			t.Errorf("служебная %q не названа в разделе служебных", c.Name)
		}
	}
}

func TestLegacyNamesArePromisedCompatible(t *testing.T) {
	// Обещание совместимости должно быть записано, иначе оно не обещание.
	for _, c := range commands() {
		if !c.Legacy {
			continue
		}
		if !strings.Contains(helpText, c.Name) {
			t.Errorf("прежнее имя %q нигде не упомянуто", c.Name)
		}
	}
	if !strings.Contains(helpText, "Прежние имена работают") {
		t.Error("в справке не сказано, что прежние имена работают")
	}
}

func TestNoTwoCommandsAnswerToTheSameName(t *testing.T) {
	// Иначе первая в таблице тихо съедает вторую.
	seen := map[string]string{}
	for _, c := range commands() {
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if was, dup := seen[n]; dup {
				t.Errorf("имя %q занято и командой %q, и %q", n, was, c.Name)
			}
			seen[n] = c.Name
		}
	}
}

func TestEveryCommandCanActuallyRun(t *testing.T) {
	// Запись в таблице без обработчика — команда, которой нет, но о которой
	// сказано в справке.
	for _, c := range commands() {
		if c.Run == nil {
			t.Errorf("у команды %q нет обработчика", c.Name)
		}
		if c.MinArgs > 0 && c.Need == "" {
			t.Errorf("команда %q требует доводов, но не говорит каких", c.Name)
		}
	}
}

func TestMissingArgumentsAreRefusedWithWhatIsNeeded(t *testing.T) {
	// Отказ должен называть недостающее, а не просто отказывать.
	for _, c := range commands() {
		if c.MinArgs == 0 {
			continue
		}
		err := dispatch(opts{}, []string{c.Name})
		if err == nil {
			t.Errorf("%q без доводов должна отказать", c.Name)
			continue
		}
		if !strings.Contains(err.Error(), c.Need) {
			t.Errorf("%q: отказ называет нужное, получено %q", c.Name, err)
		}
	}
}

func TestUnknownNameStillMeansAPaneDigest(t *testing.T) {
	// Исторически `claudex <цель>` показывает дайджест панели. Ломать это
	// нельзя: так зовут годами.
	var routed bool
	for _, c := range commands() {
		if c.Name == "не-команда-а-цель" {
			routed = true
		}
	}
	if routed {
		t.Fatal("имя занято командой — тест бессмыслен")
	}
}

func TestEveryDeclaredFlagIsDocumentedAndClassified(t *testing.T) {
	// Второй источник того же класса: флаг объявлен, но не описан, либо
	// булев и не объявлен булевым — тогда он съедает следующий довод.
	var o opts
	fs := flag.NewFlagSet("claudex", flag.ContinueOnError)
	registerFlags(fs, &o)

	var undocumented, unclassified []string
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(helpText, "--"+f.Name) {
			undocumented = append(undocumented, f.Name)
		}
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && b.IsBoolFlag() && !boolFlags[f.Name] {
			unclassified = append(unclassified, f.Name)
		}
	})
	if len(undocumented) > 0 {
		t.Errorf("флаги объявлены, но не описаны в справке: %s", strings.Join(undocumented, ", "))
	}
	if len(unclassified) > 0 {
		t.Errorf("булевы флаги не объявлены булевыми (съедят следующий довод): %s",
			strings.Join(unclassified, ", "))
	}
}
