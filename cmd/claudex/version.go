package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/surraulistic/claudex"
	"github.com/surraulistic/claudex/internal/codex"
)

// Ставятся сборкой (Makefile, install.sh) через -ldflags -X: штамп Go в
// worktree, вложенном в другой checkout, называет ревизию внешнего.
var (
	buildRevision string
	buildDirty    string
	buildDiff     string
)

// Build — откуда собран бинарь.
type Build struct {
	Path     string `json:"path"`
	Module   string `json:"module"`
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Time     string `json:"time,omitempty"`
	// Modified — собрано из рабочей копии с незакоммиченными правками.
	Modified bool `json:"modified"`
	// DiffSHA256 — отпечаток незакоммиченных правок, из которых собрано.
	DiffSHA256 string `json:"diff_sha256,omitempty"`
	// Source — откуда известна ревизия: build (сборка) или go-vcs (штамп Go).
	Source string `json:"source"`
	// GoStamp — штамп Go, если он расходится с ревизией сборки.
	GoStamp string `json:"go_vcs_stamp,omitempty"`
	Go      string `json:"go"`
	Skill   string `json:"skill_sha256"`
}

// SkillCopy — skill, который видит агент, и совпадает ли он со встроенным.
type SkillCopy struct {
	Path     string `json:"path"`
	Resolved string `json:"resolved,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Matches  bool   `json:"matches"`
	Note     string `json:"note,omitempty"`
}

func buildInfo() Build {
	b := Build{Version: "(неизвестно)", Skill: sha(claudex.Skill)}
	b.Path, _ = os.Executable()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	b.Module, b.Version, b.Go = info.Main.Path, info.Main.Version, info.GoVersion
	b.Source = "go-vcs"
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.time":
			b.Time = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}
	if buildRevision != "" {
		if b.Revision != "" && b.Revision != buildRevision {
			b.GoStamp, b.Version, b.Time = b.Revision, "", ""
		}
		b.Revision, b.Modified, b.DiffSHA256, b.Source = buildRevision, buildDirty == "true", buildDiff, "build"
	}
	return b
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// skillCopies — установленные skill claudex у агентов.
func skillCopies() []SkillCopy {
	var dirs []string
	if h := codex.Home(); h != "" {
		dirs = append(dirs, filepath.Join(h, "skills", "claudex"))
	}
	if h, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(h, ".claude", "skills", "claudex"))
	}
	want := sha(claudex.Skill)
	var out []SkillCopy
	for _, d := range dirs {
		if _, err := os.Lstat(d); err != nil {
			continue
		}
		c := SkillCopy{Path: d}
		if r, err := filepath.EvalSymlinks(d); err == nil && r != d {
			c.Resolved = r
		}
		b, err := os.ReadFile(filepath.Join(d, "SKILL.md"))
		if err != nil {
			c.Note = "SKILL.md не прочитан: " + err.Error()
			out = append(out, c)
			continue
		}
		c.SHA256 = sha(b)
		c.Matches = c.SHA256 == want
		if !c.Matches {
			c.Note = "SKILL.md не той ревизии, из которой собран бинарь"
		}
		out = append(out, c)
	}
	return out
}

func cmdVersion(o opts) error {
	b, skills := buildInfo(), skillCopies()
	if o.pretty {
		return emit(o, map[string]any{"build": b, "skills": skills})
	}
	rev := short12(b.Revision)
	dirty := ""
	if b.Modified {
		dirty = " + незакоммиченные правки"
	}
	fmt.Printf("claudex ревизия %s%s, %s\n", rev, dirty, b.Go)
	fmt.Printf("  бинарь: %s\n", b.Path)
	if b.DiffSHA256 != "" {
		fmt.Printf("  правки: sha256 %s\n", b.DiffSHA256[:16])
	}
	switch {
	case b.Source == "go-vcs":
		fmt.Println("  ⚠ ревизия — штамп Go, сборка её не записала (собрано не make/install.sh): " +
			"в worktree, вложенном в другой checkout, это ревизия внешнего")
	case b.GoStamp != "":
		fmt.Printf("  штамп Go называет %s — это внешний checkout, не источник сборки\n", b.GoStamp[:12])
	}
	for _, s := range skills {
		mark := "совпадает со сборкой"
		if !s.Matches {
			mark = "⚠ " + s.Note
		}
		where := s.Path
		if s.Resolved != "" {
			where += " → " + s.Resolved
		}
		fmt.Printf("  skill:  %s — %s\n", where, mark)
	}
	return nil
}

func short12(rev string) string {
	switch {
	case rev == "":
		return "(ревизия не записана)"
	case len(rev) > 12:
		return rev[:12]
	}
	return rev
}
