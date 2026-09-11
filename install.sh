#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin_dir="${HOME}/.local/bin"
codex_home="${CODEX_HOME:-${HOME}/.codex}"
skill_dir="${codex_home}/skills"

mkdir -p "$bin_dir" "$skill_dir"

# Бинарь копируется, а не линкуется: репозиторий может уехать на другую ветку,
# а команда должна остаться той, которую поставили.
( cd "$root" && go build -trimpath -ldflags '-s -w' -o claudex ./cmd/claudex )
install -m 0755 "${root}/claudex" "${bin_dir}/claudex"
echo "bin   → ${bin_dir}/claudex"

ln -sfn "${root}/skills/claudex" "${skill_dir}/claudex"
echo "skill → ${skill_dir}/claudex"

chmod +x "${root}/tools/"*.zsh 2>/dev/null || true
echo "tools → ${root}/tools (delegate-and-monitor.zsh, herdr-guard.zsh)"

for tool in go herdr cass; do
  command -v "$tool" >/dev/null || echo "внимание: ${tool} не найден в PATH"
done
