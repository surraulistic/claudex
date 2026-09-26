package main

import (
	"flag"
	"strings"
	"testing"
)

// Разделитель доводов не знает устройства флагов и о булевых узнаёт только из
// boolFlags. Забытый там булев флаг съедает следующий довод — и делает это
// молча: `claudex --compact brief` печатал справку, потому что `brief` уходил
// значением `--compact`. Перечислять два списка руками и не расходиться нельзя,
// поэтому согласие между ними проверяется.
func TestEveryBooleanFlagIsDeclaredBoolean(t *testing.T) {
	var o opts
	fs := flag.NewFlagSet("claudex", flag.ContinueOnError)
	registerFlags(fs, &o)

	var missed []string
	fs.VisitAll(func(f *flag.Flag) {
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && b.IsBoolFlag() && !boolFlags[f.Name] {
			missed = append(missed, f.Name)
		}
	})
	if len(missed) > 0 {
		t.Fatalf("булевы флаги не объявлены в boolFlags: %s", strings.Join(missed, ", "))
	}
}

func TestBooleanFlagDoesNotSwallowTheCommand(t *testing.T) {
	// Сам симптом: флаг перед командой.
	for _, name := range []string{"compact", "session", "no-wait"} {
		flags, rest := splitArgs([]string{"--" + name, "brief"})
		if len(rest) != 1 || rest[0] != "brief" {
			t.Fatalf("--%s: команда уцелела, получено flags=%v rest=%v", name, flags, rest)
		}
	}
}

func TestValueFlagStillTakesItsArgument(t *testing.T) {
	// Обратная сторона: небулев флаг обязан забрать следующий довод.
	flags, rest := splitArgs([]string{"find", "--limit", "3", "запрос"})
	if len(flags) != 2 || flags[1] != "3" {
		t.Fatalf("значение осталось при флаге, получено %v", flags)
	}
	if len(rest) != 2 || rest[0] != "find" || rest[1] != "запрос" {
		t.Fatalf("позиционные целы, получено %v", rest)
	}
}
