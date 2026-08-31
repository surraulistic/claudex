# claudex

Одна короткая сводка по сессии Claude Code вместо шести ручных вызовов.
Склеивает историю из Contextify с живым состоянием из Herdr.

## Установка

```bash
./install.sh
```

Ставит симлинк на бинарь в `~/.local/bin` и симлинк на скилл в
`$CODEX_HOME/skills` (по умолчанию `~/.codex/skills`). Зависимостей нет; нужны
`herdr`, `contextify`, `sqlite3` и `node` в `PATH`.

Если Codex не подхватит скилл по симлинку, скопируйте каталог:
`cp -R skills/claudex ~/.codex/skills/claudex`.

## Скилл для Codex

`skills/claudex/SKILL.md` — обычный скилл Codex в каноне
`~/.codex/skills/.system/skill-creator`. Проверка:

```bash
python3 ~/.codex/skills/.system/skill-creator/scripts/quick_validate.py skills/claudex
```

Отдельный плагин не заводился: плагин в Codex — это связка скиллов, hooks, MCP
и запись в `config.toml` (как `casino-toolkit`), а здесь одна возможность.
Если понадобится собрать всё в плагин, каталог `skills/claudex` кладётся в его
`skills/` без изменений — раскладка та же.

## Разовая настройка

Алиас — это имя агента в Herdr:

```bash
herdr agent rename wE:pB river
herdr agent rename wE:pJ config
```

`claudex sessions` показывает, что уже названо и что осталось.

Имя живёт ровно столько, сколько живёт агент в панели: Herdr снимает его, когда
агент выходит или заменяется новым. После перезапуска сессии алиас нужно
поставить заново — пропавший алиас будет виден в `unaliased`.

## Команды

```bash
claudex sessions                          # все алиасы + нерасставленные панели
claudex river                             # сводка по алиасу
claudex river --limit 12 --chars 600      # глубже
claudex river --pretty                    # с отступами, для глаз
claudex search config "MessageWhiz"       # поиск внутри транскрипта алиаса
```

Живое управление сюда не входит — это сам Herdr, по тому же имени:

```bash
herdr agent read river
herdr agent prompt river "продолжай Payment" --wait --until idle
herdr agent wait river --until idle --timeout 600000
```

## Что на выходе

```json
{
  "alias": "river",
  "live": { "pane_id": "wE:pB", "session_id": "…", "status": "working",
            "title": "…", "cwd": "…", "focused": false },
  "history": { "transcript_id": "…", "entry_count": 11900,
               "last_activity": "…", "entries": [ { "id": "…", "ts": "…",
               "role": "assistant", "text": "…", "chars": 3924, "truncated": true } ] },
  "tail": ["⏺ …"],
  "signals": { "mr": [], "tickets": [], "repo": null,
               "last_user_prompt": "…", "current_tool_call": null }
}
```

`signals` — только проверяемые извлечения. Смысловые выводы делает читатель.

У каждой записи есть `id`: если `truncated` и это важно, полный текст берётся
через `contextify entry <id>`, окружение — через `contextify context <id>`.

`tail` читается только когда панель в состоянии `working`: на простаивающей
`herdr agent read` отдаёт рамку интерфейса, а не разговор.

`history` может прийти с полем `reason` вместо `entries` — например, когда
Contextify не смог разобрать файл сессии. Это лучше, чем показать ноль записей
и сделать вид, что в сессии тихо.

## Коды выхода

`0` успех, в том числе с деградировавшей историей · `2` неизвестный алиас ·
`3` Herdr недоступен · `4` ошибка вызова.

## Тесты

```bash
node --test test/
```

## Дизайн

`docs/superpowers/specs/2026-08-31-claudex-design.md`
