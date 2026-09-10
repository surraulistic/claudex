#!/bin/zsh
# Необязательная защита от прямых `herdr agent prompt`.
#
# Отследить такой вызов задним числом нельзя: наблюдение надо начинать вместе с
# отправкой, а прямой вызов этого не делает. Поэтому здесь не «автоматическая
# регистрация», а честный отказ с указанием, чем пользоваться.
#
# Включение (осознанно, PATH-перехват настоящего herdr):
#   mkdir -p ~/.local/bin/guard
#   ln -sf <этот файл> ~/.local/bin/guard/herdr
#   export PATH="$HOME/.local/bin/guard:$PATH"
# Выключение: убрать каталог из PATH.
set -uo pipefail

real="${HERDR_REAL:-/opt/homebrew/bin/herdr}"
guard_log="${HERDR_GUARD_LOG:-/tmp/herdr-guard.log}"

if [[ "${1:-}" == "agent" && "${2:-}" == "prompt" && "${HERDR_ALLOW_RAW_PROMPT:-0}" != "1" ]]; then
  print -r -- "[$(date '+%F %T')] отклонён прямой agent prompt → ${3:-?}" >>"$guard_log"
  print -u2 "herdr agent prompt отправит задачу, но НИКТО не будет ждать её завершения:"
  print -u2 "  Codex не проснётся, в логе не появится ни строчки."
  print -u2 "Используйте единый путь:"
  print -u2 "  claudex delegate ${3:-<панель>} \"<задача>\" --notify \"<панель Codex>\""
  print -u2 "или tools/delegate-and-monitor.zsh."
  print -u2 "Осознанно и без наблюдения: HERDR_ALLOW_RAW_PROMPT=1 herdr agent prompt …"
  exit 65
fi

exec "$real" "$@"
