package exitcode

import (
	"errors"
	"fmt"
	"testing"
)

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
		"Timeout": Timeout, "Busy": Busy, "Unknown": Unknown,
	} {
		if prev, dup := seen[code]; dup {
			t.Fatalf("%s и %s делят код %d", name, prev, code)
		}
		seen[code] = name
	}
}
