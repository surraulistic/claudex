// Package exitcode — коды выхода как договор с вызывающим. Ведущий различает
// «панель занята» и «сбой»: в первом случае можно подождать и повторить,
// во втором — нет.
package exitcode

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	Fail     = 1 // общий сбой
	NotFound = 2 // цель не найдена
	NoHerdr  = 3 // herdr недоступен
	BadCall  = 4 // ошибка вызова
	Timeout  = 5 // не дождался
	Busy     = 6 // панель занята, задание не отправлено
	Unknown  = 7 // сбой herdr во время ожидания — исход неизвестен
	NotArmed = 8 // панель не начала работу за отведённое время
)

type coded struct {
	code int
	err  error
}

func (c coded) Error() string { return c.err.Error() }
func (c coded) Unwrap() error { return c.err }

func Wrap(code int, err error) error {
	if err == nil {
		return nil
	}
	return coded{code: code, err: err}
}

func Errorf(code int, format string, a ...any) error {
	return Wrap(code, fmt.Errorf(format, a...))
}

// Of достаёт код сквозь любое число обёрток.
func Of(err error) int {
	if err == nil {
		return 0
	}
	var c coded
	if errors.As(err, &c) {
		return c.code
	}
	return Fail
}

// Duration читает и голые секунды, и человеческий срок: прежняя версия
// принимала «1800», ломать это нельзя.
func Duration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("непонятный срок %q: нужны секунды числом или вид 30m", s)
	}
	return d, nil
}
