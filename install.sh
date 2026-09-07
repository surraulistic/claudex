#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin_dir="${HOME}/.local/bin"
codex_home="${CODEX_HOME:-${HOME}/.codex}"
skill_dir="${codex_home}/skills"

mkdir -p "$bin_dir" "$skill_dir"

ln -sfn "${root}/bin/claudex" "${bin_dir}/claudex"
echo "bin   → ${bin_dir}/claudex"

ln -sfn "${root}/skills/claudex" "${skill_dir}/claudex"
echo "skill → ${skill_dir}/claudex"

for tool in herdr cass sqlite3 node; do
  command -v "$tool" >/dev/null || echo "внимание: ${tool} не найден в PATH"
done
