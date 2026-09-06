# claudex

Одна короткая сводка по сессиям Claude Code вместо шести ручных вызовов.
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

## Адресация

Цель — алиас Herdr, `pane_id`, кусок заголовка панели или cwd. Настройка не
нужна: у каждой панели в `sessions` есть поле `target`, которое гарантированно
работает, и для безымянной панели это её `pane_id`.

```bash
claudex wE:p13              # по панели
claudex "кеш инвалидация"   # по заголовку
```

Алиас — необязательное удобство, живёт он в самом Herdr и снимается, когда агент
в панели выходит или заменяется новым:

```bash
herdr agent rename wE:pB river
```

## Команды

```bash
claudex brief                             # все живые панели разом — основной вход
claudex sessions                          # легче: кто жив и у кого есть история
claudex wE:p13                            # полный дайджест одной панели
claudex river --limit 12 --chars 600      # глубже
claudex river --pretty                    # с отступами, для глаз
claudex search wE:p9 "MessageWhiz"        # поиск внутри одной сессии
claudex find "river миграция"             # поиск по всем транскриптам
claudex find "wallet currency" --days 14  # то же, но не старше двух недель
```

`brief` укладывает все панели в один процесс: старт node и обращение к базе
амортизируются, хвосты читаются параллельно. Семь отдельных дайджестов стоят
впятеро дороже той же картины.

`find` ходит по всем транскриптам, включая давно закрытые сессии, и возвращает
`session_id`, `cwd` и ветку — по ним видно, куда смотреть дальше. Слова
объединяются через AND, хвостовая `*` даёт префикс, `--raw` пропускает синтаксис
FTS5 как есть.

Живое управление сюда не входит — это сам Herdr, по тому же имени:

```bash
herdr agent read river
herdr agent prompt river "продолжай Payment" --wait --until idle
herdr agent wait river --until idle --timeout 600000
```

## Что на выходе

```json
{
  "target": "river",
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

`tail` читается при любом статусе: `--source recent` отдаёт скроллбэк, а не поле
ввода, и на завершённой панели там как раз итоговый ответ.

Сигналы с узкой панели намеренно скупые. Ужатая статусная строка режет id и в
середине списка — `SD-6613` приезжает как `SD-66`, которого не существует, —
поэтому такие строки пропускаются целиком. Пустой `mr` означает «на экране нет
ничего проверяемого», а не «нет MR».

`history` может прийти с полем `reason` вместо `entries` — например, когда
Contextify не смог разобрать файл сессии. Это лучше, чем показать ноль записей
и сделать вид, что в сессии тихо.

## Коды выхода

`0` успех, в том числе с деградировавшей историей · `2` цель не найдена или
неоднозначна · `3` Herdr недоступен · `4` ошибка вызова.

## Тесты

```bash
node --test test/
```

## Дизайн

`docs/superpowers/specs/2026-08-31-claudex-design.md`
