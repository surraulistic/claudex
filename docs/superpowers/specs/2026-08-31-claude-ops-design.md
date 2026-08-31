# claude-ops — дизайн

Дата: 2026-08-31. Статус: утверждён, готов к плану реализации.

## Задача

Codex ведёт несколько параллельных сессий Claude Code (river, config,
entitlements, bugs). Чтобы понять состояние одной из них, он вручную делает
шесть вызовов: ищет транскрипт в Contextify, читает activity, ищет панель в
Herdr, читает tail, иногда шлёт prompt, затем склеивает историю с живым
состоянием.

Замерено 2026-08-31 на реальных данных, сырьё на **один** алиас:

| источник | байт |
|---|---:|
| `contextify … --json transcripts --limit 10` | 2 729 |
| `contextify … --json activity --limit 8` | 8 881 |
| `herdr agent list` | 4 727 |
| `herdr agent read <pane>` | 4 835 |
| **итого** | **21 172** |

На три алиаса — около 63 КБ, и это до того, как из них извлечён смысл.

## Что уже готово и не нужно писать

Разведка окружения дала четыре факта, каждый из которых убирает кусок
исходного замысла.

**Herdr умеет алиасы сам.** Agent-команды принимают либо pane id, либо
уникальное живое имя агента (`[a-z][a-z0-9_-]{0,31}`). После разового
`herdr agent rename wE:pB river` работают `herdr agent read river`,
`herdr agent prompt river "…" --wait --until idle`,
`herdr agent wait river --until idle --timeout 600000`. Собственная
alias-карта для живой части не нужна, как и команды `read`, `ask`, `wait`.

**Мост между системами существует.** В `contextify.db` колонка
`transcripts.provider_session_id` равна нативному session id Claude Code,
который Herdr отдаёт в `agent_session.value`. Соответствие проверено на всех
живых сессиях проекта. На паре `(provider, provider_session_id)` стоит
`UNIQUE INDEX uq_tr_provider_session`. CLI Contextify это поле не отдаёт, и
`--transcript-id` не принимает ни session id, ни slug.

**Свои hooks не нужны.** Herdr уже ставит `~/.claude/hooks/herdr-agent-state.sh`
на `SessionStart`; хук шлёт `session_id` и `transcript_path` серверу Herdr по
сокету. Ещё один хук дублировал бы это и добавил запись в `settings.json`, где
уже семь событий от moshi и herdr, причём herdr перезаписывает свой файл при
обновлении.

**Contextify уже режет.** `activity` отдаёт только timeline-записи
(`user`/`assistant`), tool-выхлоп в выдачу не попадает, контент обрезается на
2 КБ с флагом `contentTruncated`.

Тупики: `contextify fleet` работает на уровне проекта, а не сессии;
`transcript_summaries` пуста.

## Границы

Инструмент делает три вещи, которых нет ни у одной из систем:

1. резолвит алиас в тройку `(pane_id, session_id, transcript_id)`;
2. склеивает историю Contextify с живым состоянием Herdr и режет результат;
3. даёт Codex одну точку входа вместо шести вызовов.

Не делает: не оборачивает `read`/`ask`/`wait`, не хранит состояние, не ставит
hooks, не читает `~/.claude/*.jsonl`, не ходит в сеть.

Алиас определяется именем агента в Herdr. Разовая настройка:

```bash
herdr agent rename wE:pB river
herdr agent rename wE:pJ config
herdr agent rename wE:pM entitlements
herdr agent rename wE:pK bugs
```

Файла состояния нет намеренно. Сессии живут неделями (river — 11 812 записей
за шесть дней), и вся работа по теме лежит в одном транскрипте: 534 упоминания
River в `1FB2817C`, у остальных транскриптов проекта единицы. Сшивать историю
через мёртвые сессии нечего, поэтому алиас резолвится живьём при каждом вызове
и протухать нечему.

## Поток данных

```
alias
 ├─ herdr agent get <alias>             → pane_id, session_id, status,
 │                                         title, cwd, focused
 ├─ sqlite3 -readonly contextify.db     → transcript_id
 │    select id from transcripts
 │    where provider_session_id = ? and provider = 'claude.code'
 ├─ contextify --transcript-id … activity → история, режется клиентом
 └─ herdr agent read <alias>            → хвост, только при status=working
```

Асимметрия принципиальная: Herdr отвечает на «что происходит сейчас»,
Contextify — на «что уже сделано». На простаивающей панели `herdr agent read`
отдаёт 4.8 КБ рамок и пустое поле ввода, содержимого разговора там нет; на
работающей — узкий полезный хвост (строки `⏺`, текущий tool call, статус-бар с
веткой и тикетами). Поэтому хвост читается только при `status=working`.

## Контракт вывода

`digest` печатает один JSON-объект в stdout.

```json
{
  "alias": "river",
  "live": {
    "pane_id": "wE:pB",
    "session_id": "846a1bcf-bad5-4ffa-8366-9c5e31205ac7",
    "status": "idle",
    "title": "Centrifugo vs riverqueue",
    "cwd": "/Users/surraulistic/GolandProjects",
    "focused": false
  },
  "history": {
    "transcript_id": "1FB2817C-39D3-4CF3-A795-0D5BF906FC07",
    "provider": "claude.code",
    "entry_count": 11812,
    "last_activity": "2026-08-31T11:18:58+03:00",
    "entries": [
      {
        "id": "0df863d3-4f56-45f7-b686-5de99a764cd3",
        "ts": "2026-08-31T11:18:58+03:00",
        "role": "assistant",
        "text": "bonuses!874/!873 влиты, живая проверка прошла…",
        "chars": 3924,
        "truncated": true
      }
    ]
  },
  "tail": null,
  "signals": {
    "mr": ["bonuses!874", "!873"],
    "tickets": ["SD-8208"],
    "branch": null,
    "last_user_prompt": "продолжай",
    "current_tool_call": null
  }
}
```

`entries` идут от новых к старым, как их отдаёт Contextify. `tail` — массив
строк либо `null`, если панель не в состоянии `working`. `history` — объект
либо `null` с полем `reason`.

### Почему `signals`, а не `state`

Исходный замысел предполагал блок `state` с полями `done`, `blockers`,
`needs_decision`, `next_step`, вычисляемыми эвристикой. Отклонено: эвристика по
русскому тексту будет ошибаться, а Codex всё равно перечитает записи, чтобы
убедиться, — это больше токенов, а не меньше. `signals` содержит только
детерминированные извлечения, которые Codex не станет перепроверять.

| поле | как получено |
|---|---|
| `mr` | регулярное выражение по тексту записей: `[\w./-]+![0-9]+` и голое `![0-9]{2,}`. Извлекается ровно то, что написано в тексте: из «bonuses!874/!873» получится `["bonuses!874", "!873"]`. Достраивать полное имя проекта инструмент не может и не пытается |
| `tickets` | `\b(?:SNEW\|SD\|BF)-\d+\b` |
| `branch` | из статус-бара хвоста; `null`, когда хвост не читался |
| `last_user_prompt` | последняя запись с `role = "user"` в выборке |
| `current_tool_call` | строка хвоста вида `⏺ Running …` или `$ …`; `null` без хвоста |

Списки дедуплицируются и обрезаются: по 10 элементов в `mr` и `tickets`.

Смысловые выводы («MR влит», «нужен decision», «блокер внешний») делает Codex —
ровно как в исходной схеме потока.

### Углубление

Каждая запись несёт свой `id`. Увидев `truncated: true` на важной записи, Codex
сам добирает полный текст через `contextify entry <uuid>` или окружение через
`contextify context <uuid>`. Инструмент не угадывает, что важно: он даёт
короткое и ручку.

### Резка

- текст записи обрезается до 400 символов (`--chars N`), исходная длина
  сохраняется в `chars`, факт обрезки — в `truncated`;
- переводы строк внутри текста схлопываются в пробел;
- хвост: убираются строки из одних рамок (`─ ━ ╌`), футер Claude Code (полосы
  прогресса `█ ░`, строка `⏵⏵ auto mode`), хвостовые пробелы и пустые строки;
  остаются последние 12 значимых строк (`--tail-lines N`).

Целевой размер — не более 3 КБ на алиас против нынешних 21 КБ. Это критерий
приёмки, а не пожелание.

## Реализация

Node без npm-зависимостей, `#!/usr/bin/env node`, вызовы `herdr`, `contextify`
и `sqlite3` через `child_process`. Все три бинаря есть в PATH.

Go отклонён: нужен sqlite-драйвер, а действующее правило требует собирать Go на
dev-сервере — для инструмента, который правится по десять раз за вечер, это
лишнее трение. `bash` + `jq` отклонены: сборка JSON в shell даёт тихие ошибки.
`node:sqlite` недоступен — установлен Node 20.19.6, модуль появился в 22.5,
поэтому вызывается бинарь `sqlite3 -readonly -json`.

Путь к базе: `--db-path`, иначе `CLAUDE_OPS_CONTEXTIFY_DB`, иначе
`~/Documents/Contextify/contextify.db`.

Расположение: `~/projects/claude-ops`, симлинк в `~/.local/bin/claude-ops`.
Не в `~/GolandProjects` — там лежат 22 сервисных репозитория.

### Деградация

Прямой SELECT по `contextify.db` — единственное место, опирающееся на
недокументированную схему. Если база недоступна, запрос падает или строка не
найдена, `history` возвращается как `null` с полем `reason`, а `live`,
`tail` и `signals` отдаются как обычно; код выхода 0. Инструмент деградирует,
а не ломается.

Отдельно будет отправлен `contextify feedback` с просьбой отдавать
`provider_session_id` в выводе `--json transcripts`. Когда это появится, SQL
уходит из инструмента совсем.

### Безопасность

Только чтение, `sqlite3 -readonly`. В вывод не попадают ни `file_path`
транскрипта, ни содержимое tool-выхлопа — Contextify его и не отдаёт. Сетевых
вызовов нет. Файлы `~/.claude/*.jsonl` не читаются.

## Команды MVP

| # | команда | назначение |
|---|---|---|
| 1 | `claude-ops sessions` | резолвит все алиасы разом; проверяет мост целиком |
| 2 | `claude-ops digest <alias> [--limit N] [--chars N] [--tail-lines N]` | основная ценность |

Умолчания: `--limit 8` записей, `--chars 400` символов на запись,
`--tail-lines 12` строк хвоста. `search` печатает те же поля записи, что и
`digest`, плюс `score` из Contextify.
| 3 | `claude-ops search <alias> "<запрос>"` | `contextify search` со скоупом транскрипта |

`sessions` дополнительно перечисляет агентов без имени (`pane_id` и заголовок),
чтобы было видно, что осталось переименовать.

Вне MVP и не планируется: `status` и `activity` — подмножества `digest`;
`read`, `ask`, `wait` — это `herdr agent read|prompt|wait <alias>` один в один.

### Коды выхода

`0` — успех, в том числе когда `history` деградировал до `null`.
`2` — неизвестный алиас.
`3` — Herdr недоступен, живое состояние получить нечем.

## Критерии приёмки

1. `claude-ops sessions` возвращает четыре алиаса, у каждого разрешён
   `transcript_id`; агенты без имени перечислены отдельно.
2. `claude-ops digest river` укладывается в 3 КБ и содержит
   `transcript_id` `1FB2817C-39D3-4CF3-A795-0D5BF906FC07`, а каждая запись — свой `id`.
3. `claude-ops digest bugs` при работающей панели содержит непустой `tail`,
   `signals.branch` и `signals.current_tool_call`.
4. `--db-path` на несуществующий файл: код выхода 0, `history` равен `null` с
   заполненным `reason`, `live` на месте.
5. Неизвестный алиас: код выхода 2 и сообщение со списком известных алиасов.
6. Вывод `digest` — валидный JSON при любом из перечисленных исходов.
