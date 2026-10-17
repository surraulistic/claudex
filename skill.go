// Package claudex встраивает skill в бинарь: по нему видно, той ли ревизии
// skill, что лежит у агентов.
package claudex

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"sort"
)

//go:embed skills/claudex
var skill embed.FS

// SkillFS — встроенный каталог skill.
func SkillFS() fs.FS {
	sub, _ := fs.Sub(skill, "skills/claudex")
	return sub
}

// SkillHash — отпечаток каталога skill: пути и содержимое всех файлов.
func SkillHash(dir fs.FS) (string, error) {
	var paths []string
	err := fs.WalkDir(dir, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && d.Name() != ".DS_Store" {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(dir, p)
		if err != nil {
			return "", err
		}
		h.Write([]byte(p + "\x00"))
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
