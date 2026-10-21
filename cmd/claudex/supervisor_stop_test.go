package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
