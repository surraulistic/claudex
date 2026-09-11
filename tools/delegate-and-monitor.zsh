#!/bin/zsh
# ЕДИНСТВЕННЫЙ рекомендуемый способ поручить задачу панели Claude.
#
# Почему не прямой `herdr agent prompt`: он отправляет промпт и на этом всё.
# Никто не ждёт завершения, Codex не будет разбужен, в логе не появится ни
# строчки. Именно так и вышло в прошлый раз.
#
# Здесь отправка и наблюдение — один процесс claudex delegate: промпт уходит и
# ожидание начинается без промежутка, поэтому доказывать привязку завершения к
# задаче нечем — окна между ними нет.
#
#   ./delegate-and-monitor.zsh install "задача"
#   CODEX_TARGET="codex install" ./delegate-and-monitor.zsh install "задача"
#
# Постоянно работающего процесса здесь нет и не обещается: наблюдатель живёт
# ровно одну задачу. Supervisor не нужен, потому что нечего перезапускать.
set -uo pipefail

target="${1:-}"
task="${2:-}"
if [[ -z "$target" || -z "$task" ]]; then
  print -u2 "использование: $0 <метка-панели> \"<задача>\" [доп. флаги claudex]"
  print -u2 "  метки панелей: claudex sessions"
  exit 64
fi
shift 2

timeout_s="${DELEGATE_TIMEOUT_S:-7200}"
log="${DELEGATE_LOG:-/tmp/delegate-and-monitor.log}"

if ! command -v claudex >/dev/null; then
  print -u2 "claudex не найден в PATH — без него задача не будет отслежена"
  exit 69
fi

# Ждать, пока панель возьмётся за дело, умеет сам herdr: отправка и ожидание
# уходят одним вызовом, промежутка между ними нет.
args=(delegate "$target" "$task" --timeout "$timeout_s" --pretty)
[[ -n "${CODEX_TARGET:-}" ]] && args+=(--notify "$CODEX_TARGET")
args+=("$@")

stamp() { print -r -- "[$(date '+%F %T')] $*" }
stamp "delegate → ${target}: ${task}" >>"$log"

claudex "${args[@]}" 2>&1 | tee -a "$log"
rc="${pipestatus[1]}"

case "$rc" in
  0) reason="панель завершила задачу" ;;
  2) reason="панель не найдена — проверьте claudex sessions" ;;
  3) reason="herdr недоступен" ;;
  4) reason="ошибка вызова" ;;
  5) reason="не дождались за ${timeout_s} с; задача могла продолжаться" ;;
  6) reason="панель занята или уже под наблюдением — промпт НЕ отправлен" ;;
  7) reason="сбой herdr при ожидании — исход неизвестен, проверьте панель вручную" ;;
  64) reason="неверные аргументы" ;;
  69) reason="claudex недоступен" ;;
  *) reason="неожиданный код" ;;
esac
stamp "код ${rc}: ${reason}" | tee -a "$log" >&2
exit "$rc"
