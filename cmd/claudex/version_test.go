package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/surraulistic/claudex"
)

func TestBuildRevisionWinsOverTheOuterCheckoutStamp(t *testing.T) {
	prev := buildRevision
	buildRevision, buildDirty = "bc10e76cab403c5434f72e021bc34b51bba8c9cc", "true"
	t.Cleanup(func() { buildRevision, buildDirty = prev, "" })
	b := buildInfo()
	if b.Revision != buildRevision || !b.Modified || b.Source != "build" {
		t.Fatalf("ревизия — та, что записала сборка, с признаком правок: %+v", b)
	}
}

func TestUnstampedBuildSaysItsRevisionIsAGuess(t *testing.T) {
	if buildInfo().Source != "go-vcs" {
		t.Fatal("без записи сборки источник ревизии назван штампом Go")
	}
}

func TestSkillOfAnotherRevisionIsReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	same := filepath.Join(home, ".codex", "skills", "claudex")
	os.MkdirAll(same, 0o755)
	os.WriteFile(filepath.Join(same, "SKILL.md"), claudex.Skill, 0o644)
	other := filepath.Join(home, ".claude", "skills", "claudex")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "SKILL.md"), []byte("прежний skill"), 0o644)

	got := skillCopies()
	if len(got) != 2 || !got[0].Matches || got[1].Matches || got[1].Note == "" {
		t.Fatalf("та же ревизия совпадает, другая названа: %+v", got)
	}
}

func TestBadFlagDoesNotPrintTheWholeHelp(t *testing.T) {
	var o opts
	var out bytes.Buffer
	fs := flag.NewFlagSet("claudex", flag.ContinueOnError)
	registerFlags(fs, &o)
	fs.SetOutput(&out)
	r, w, _ := os.Pipe()
	stderr := os.Stderr
	os.Stderr = w
	err := fs.Parse([]string{"--bogus"})
	w.Close()
	os.Stderr = stderr
	var printed bytes.Buffer
	printed.ReadFrom(r)
	if err == nil {
		t.Fatal("неизвестный флаг — ошибка")
	}
	if n := out.Len() + printed.Len(); n > 300 {
		t.Fatalf("опечатка во флаге стоит строки, а не справки: %d байт", n)
	}
}
