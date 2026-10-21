package main

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/surraulistic/claudex/internal/supervise"
)

func TestStopNeverSignalsAPidTheLockDoesNotVouchFor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	// Файл состояния называет живой, но чужой процесс: так выглядит
	// переиспользованный pid после смерти наблюдателя.
	b, _ := json.Marshal(supervise.Status{PID: other.Process.Pid})
	os.MkdirAll(filepath.Dir(supervise.StatusPath()), 0o755)
	os.WriteFile(supervise.StatusPath(), b, 0o644)

	if err := supervisorStop(); err == nil {
		t.Fatal("без замка остановка — отказ с причиной")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("чужой процесс жив: %v", err)
	}
}

func TestLockRecordsItsHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "supervisor.lock")
	release, err := supervise.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if pid, held := supervise.Holder(path); !held || pid != os.Getpid() {
		t.Fatalf("замок называет держателя, получено %d %v", pid, held)
	}
}

func TestSupervisorStartsDespiteAStaleStatusWithALivePID(t *testing.T) {
	// F4: файл состояния называет живой чужой процесс, замок свободен —
	// наблюдатель обязан подняться, а автоподъём — не считать его работающим.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "нет.sock"))
	b, _ := json.Marshal(supervise.Status{PID: os.Getppid(), Ticks: 7639})
	os.MkdirAll(filepath.Dir(supervise.StatusPath()), 0o755)
	os.WriteFile(supervise.StatusPath(), b, 0o644)

	if supervisorRunning() {
		t.Fatal("без замка наблюдатель не работает, какой бы pid ни был в файле")
	}
	if err := cmdSupervisor(opts{once: true}, nil); err != nil {
		t.Fatalf("наблюдатель поднимается, получено %v", err)
	}
	st, _ := supervise.Read(supervise.StatusPath())
	if st.PID != os.Getpid() || st.Ticks != 1 {
		t.Fatalf("проход сделан и записан своим pid, получено %+v", st)
	}
}

func TestAutostartKeepsTheFlagsOfTheLastExplicitStart(t *testing.T) {
	// F9: наблюдатель, запущенный с --silence-grace 20m --archive-after 30,
	// после падения поднимается с теми же флагами, а не с умолчаниями.
	t.Setenv("HOME", t.TempDir())
	if got := loadSupervisorArgs(); got != nil {
		t.Fatalf("без явного запуска — умолчания, получено %v", got)
	}
	o := opts{intervalRaw: "60", silenceRaw: "1h30m", maxPerTick: 5, archiveRaw: "30", reconcile: true}
	if err := saveSupervisorArgs(supervisorFlags(o)); err != nil {
		t.Fatal(err)
	}
	want := []string{"--interval", "60", "--silence-grace", "1h30m", "--max-per-tick", "5",
		"--archive-after", "30", "--reconcile"}
	if got := loadSupervisorArgs(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("автоподъём берёт сохранённое, получено %v", got)
	}
	// Флаги действительно разбираются так же, как в командной строке.
	var back opts
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	registerFlags(fs, &back)
	if err := fs.Parse(want); err != nil || back.silenceRaw != "1h30m" || !back.reconcile || back.maxPerTick != 5 {
		t.Fatalf("сохранённое — корректные флаги, получено %+v %v", back, err)
	}
}

func TestTamperedSupervisorArgsAreIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, bad := range [][]string{
		{"--interval", "30; rm -rf ~"},
		{"--journal", "/tmp/x"},
		{"--interval"},
	} {
		if err := saveSupervisorArgs(bad); err != nil {
			t.Fatal(err)
		}
		if got := loadSupervisorArgs(); got != nil {
			t.Fatalf("%v не становится argv, получено %v", bad, got)
		}
	}
}
