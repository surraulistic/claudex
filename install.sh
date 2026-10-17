#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin_dir="${HOME}/.local/bin"
codex_home="${CODEX_HOME:-${HOME}/.codex}"
skill_dir="${codex_home}/skills"

mkdir -p "$bin_dir" "$skill_dir"

# Бинарь копируется, а не линкуется: репозиторий может уехать на другую ветку,
# а команда должна остаться той, которую поставили.
# Ревизия пишется явно: штамп Go в worktree, вложенном в другой checkout,
# называет ревизию внешнего.
rev="$(git -C "$root" rev-parse HEAD)"
dirty=false
[ -z "$(git -C "$root" status --porcelain)" ] || dirty=true
diff="$( { git -C "$root" diff HEAD; git -C "$root" ls-files --others --exclude-standard -z |
  (cd "$root" && xargs -0 cat 2>/dev/null); } | shasum -a 256 | cut -c1-64)"
( cd "$root" && go build -trimpath \
  -ldflags "-s -w -X main.buildRevision=${rev} -X main.buildDirty=${dirty} -X main.buildDiff=${diff}" \
  -o claudex ./cmd/claudex )
install -m 0755 "${root}/claudex" "${bin_dir}/claudex"
echo "bin   → ${bin_dir}/claudex (${rev:0:12}$([ "$dirty" = true ] && echo ' + правки'))"

# Skill копируется по той же причине, что и бинарь: ссылка на checkout
# следовала бы его ветке, и skill расходился бы с поставленной командой.
rm -rf "${skill_dir}/claudex"
cp -R "${root}/skills/claudex" "${skill_dir}/claudex"
echo "skill → ${skill_dir}/claudex (копия; claudex version сверяет её с бинарём)"

chmod +x "${root}/tools/"*.zsh 2>/dev/null || true
echo "tools → ${root}/tools (delegate-and-monitor.zsh, herdr-guard.zsh)"

for tool in go herdr cass; do
  command -v "$tool" >/dev/null || echo "внимание: ${tool} не найден в PATH"
done
