// Package claudex встраивает skill в бинарь: по нему видно, той ли ревизии
// skill, что лежит у агентов.
package claudex

import _ "embed"

//go:embed skills/claudex/SKILL.md
var Skill []byte
