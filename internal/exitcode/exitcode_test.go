package exitcode

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSecondsAndDurationsBothParse(t *testing.T) {
	// Прежняя версия принимала секунды числом; ломать это нельзя, но и
	// «30m» читать удобно.
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"1800", 30 * time.Minute},
		{"30m", 30 * time.Minute},
		{"90s", 90 * time.Second},
		{"0", 0},
	} {
		got, err := Duration(c.in)
		if err != nil || got != c.want {
			t.Fatalf("Duration(%q) = %v, %v; ожидалось %v", c.in, got, err, c.want)
		}
	}
	if _, err := Duration("скоро"); err == nil {
		t.Fatal("невнятный срок — ошибка")
	}
}

func TestCodesAreCarriedThroughWrapping(t *testing.T) {
	// Код должен переживать обёртывание: иначе вызывающий увидит 1 вместо
	// «панель занята» и сочтёт поручение проваленным.
	err := fmt.Errorf("не вышло: %w", Wrap(Busy, errors.New("занята")))
	if got := Of(err); got != Busy {
		t.Fatalf("код сквозь обёртку, получено %d", got)
	}
}

func TestUnknownErrorIsGenericFailure(t *testing.T) {
	if got := Of(errors.New("что-то")); got != Fail {
		t.Fatalf("неизвестная ошибка — общий сбой, получено %d", got)
	}
	if got := Of(nil); got != 0 {
		t.Fatalf("нет ошибки — нет кода, получено %d", got)
	}
}

func TestEveryCodeHasItsOwnMeaning(t *testing.T) {
	seen := map[int]string{}
	for name, code := range map[string]int{
		"Fail": Fail, "NotFound": NotFound, "NoHerdr": NoHerdr, "BadCall": BadCall,
		"Timeout": Timeout, "Busy": Busy, "Unknown": Unknown, "NotArmed": NotArmed,
	} {
		if prev, dup := seen[code]; dup {
			t.Fatalf("%s и %s делят код %d", name, prev, code)
		}
		seen[code] = name
	}
}
