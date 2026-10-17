# Пробуждение треда Codex через app-server (поручение afc2c22d)

Ветка `codex-wake-appserver` от `a1cc918`, worktree
`.claude/worktrees/codex-wake-appserver`. Закоммичено и запушено в
`origin/codex-wake-appserver` (поручение 242fb014); в main не влито, не
установлено.

## Проблема

`codex queue` на выгруженном треде только сохраняет сообщение — ход не
начинается (openai/codex#44491, мейнтейнер: так задумано, советует queue +
resume). claudex считал «очередь приняла» доставкой.

## Факты протокола (Codex CLI / app-server 0.162.1, проверено 2026-10-10)

Всё — на отдельном тестовом треде `01a1265e…`, созданном для проверки
(после — заархивирован). Пользовательские треды не resume-ились.

- Управляющий сокет `~/.codex/app-server-control/app-server-control.sock` —
  **WebSocket поверх unix-сокета**, JSON-RPC (`initialize` → `initialized`).
  `codex app-server proxy` с сырыми JSON-строками не работает (broken pipe).
- Нужен `capabilities.experimentalApi: true` для `thread/queue/*`.
- `thread/read` → `status.type`: `notLoaded` | `idle` | `active`
  (`activeFlags`: `waitingOnApproval`, `waitingOnUserInput`) | `systemError`.
  Флага «paused» в протоколе нет.
- `thread/queue/add` на **загруженном idle** треде стартует ход сразу.
- На **notLoaded** — только сохраняет; `thread/resume` с явным permission profile
  **сам вычитывает очередь** и стартует ход. `thread/queue/start` по id нужен
  только как запасной.
- Сервер **не** дедуплицирует `clientUserMessageId`; зато он виден в
  `thread/turns/list` (itemsView=summary) как `userMessage.clientId` — по нему
  опознаётся «наш» ход и его статус, без подписки и без токенов.
- Ход переживает отключение того, кто его разбудил; после конца хода тред
  снова выгружается.
- Файл `thread-writer-locks/<id>.lock` есть только у загруженного треда; если
  демон говорит `notLoaded`, а замок есть — тред держит другой процесс.
- Переопределения в `thread/resume` липкие. Холодный resume явно переводит тред
  на permission profile `claudex-work`; это намеренно, чтобы не унаследовать
  старый Full Access. Остальные настройки (модель, approval policy) не меняются.
- Цена хода: базовая ≈23k input даже у пустого треда (системный промпт +
  инструменты), из них большая часть из кэша; ход на тестовом треде —
  25.7k input / 22.3k cached. Дешевле «исходный тред обработал событие» не
  бывает: история треда читается моделью. Экономия против `codex exec resume`
  — нет нового процесса и MCP-старта, кэш демона тёплый, вход — короткое
  событие, а не отчёт.

- Схема (`codex app-server generate-json-schema --experimental`, 0.162.1):
  `thread/resume.permissions: string` — «named profile id», нельзя вместе с
  `sandbox`. Поле **экспериментальное**: в stable-схеме его нет, как и
  `activePermissionProfile` в ответе. В ответе resume —
  `activePermissionProfile.id` (nullable, «when known») и устаревший
  `sandbox.type`. `permissionProfile/list` (stable, `cwd` опционален) отдаёт
  `{id, allowed}`.
- Та же схема: `thread/settings/update` (experimental) принимает
  `permissions: string|null` — «named permissions profile id for subsequent
  turns», нельзя вместе с `sandboxPolicy`; ответ — пустой объект. Применённое
  приходит server notification `thread/settings/updated` с `threadId` и
  `threadSettings` (`activePermissionProfile: {id, extends}|null`,
  `sandboxPolicy.type`: `dangerFullAccess` | `readOnly` | `externalSandbox` |
  `workspaceWrite`). Уведомление получают **только подписчики треда** и
  **только при фактической смене** настроек — см. раздел поручения b0729eba.
- `~/.codex/config.toml`: `default_permissions = "claudex-work"`,
  `sandbox_mode` не задан, `approval_policy = "never"`. Дефолт конфига влияет
  только на новые треды; старый тред с Full Access держит его, пока resume не
  переопределит (вывод из липкости resume, вживую на Full Access-треде не
  проверялось).

## Решение

`internal/codex`: свой минимальный RFC 6455-клиент (`ws.go`, без новых
зависимостей), JSON-RPC-клиент (`appserver.go`), алгоритм `Wake`/`Follow`
(`wake.go`), поддельный app-server `codextest` (настоящий WS по unix-сокету).

`Wake`: read → поиск своего хода → поиск своей записи в очереди (по
`clientUserMessageId` = `claudex-<task>-<stage>`, или по тексту для положенного
`codex queue`) → по статусу: active/approval/systemError/неизвестный — Hold, ничего
не трогаем; чужие сообщения в очереди — запрет; `permissionProfile/list(cwd)`.
Дальше ветки:

- **idle (загруженный)**: `thread/resume {permissions: claudex-work}` —
  подписка на тред и действующий профиль в ответе. Уже `claudex-work` с
  `sandbox.type` ∈ {`readOnly`, `workspaceWrite`} → дальше без update. Иначе
  `thread/settings/update {threadId, permissions: claudex-work}` → ack → ждём
  `thread/settings/updated` именно этого `threadId` (в пределах `Each`;
  пришедшее до ответа тоже считается, до запроса — нет) →
  `activePermissionProfile.id == claudex-work` и `sandboxPolicy.type` ∈
  {`readOnly`, `workspaceWrite`} → повторный read (idle) → `queue/add` → тред
  берёт сам, ждём ход. `queue/start` здесь не зовётся никогда: не взял сам —
  `turn_not_started`.
- **notLoaded**: запрет при чужом писателе → resume с `claudex-work` →
  проверка профиля → read → `queue/add` (если журнал не знает о принятии) →
  ожидание хода → `queue/start` своего id, только если он первый.

App-server недоступен → `codex queue` и `queue_only`.

`internal/task`: стадии в журнале (`thread_stage`, `transport`, `queue_id`,
`turn_id`, `thread_client_id`); новое состояние `queued`; принятое не считается
доставкой (`ReachedLeader` только для начатого хода) — дожим повторяет
пробуждение, но не сообщение. `FollowTurns` отмечает завершение хода.

Наблюдатель: замок `~/.claudex/locks/supervisor.lock`; автозапуск из любого
`send`/`delegate`/`done`; каждый тик — `FollowTurns`.

## Тупики

- `codex app-server proxy` + сырые JSON-строки: сокет ждёт WS-рукопожатие.
- Переопределить sandbox на время хода: в протоколе нет «только для этого
  хода» — всё липкое.

## Ограничения

- Fail-closed по профилю: перед resume — `permissionProfile/list` с `cwd`
  треда; профиля `claudex-work` нет, он `allowed: false` или список не
  получен → `permission_profile_unavailable`, resume не делается. После
  resume `activePermissionProfile.id` обязан быть `claudex-work`, а
  `sandbox.type` — не `dangerFullAccess`, иначе `permission_profile_mismatch`
  и `queue/start` не вызывается. Вторая проверка не предотвращает ход:
  resume сам вычитывает очередь — гейт это первая проверка.
- Загруженный idle тред (поручения 840c0d1a, b0729eba): до `queue/add` —
  resume и, если профиль не тот, `thread/settings/update` с подтверждением
  уведомлением, см. «Решение». Ошибка resume или update, нет уведомления за
  `Each`, уведомление про другой тред → `loaded_thread_profile_unconfirmed`;
  не тот профиль, `null`,
  `dangerFullAccess` или `externalSandbox` (полный доступ к диску) →
  `permission_profile_mismatch`. В обоих случаях стадия `not_queued`, ни
  `queue/add`, ни `queue/start`. `externalSandbox` отвергается намеренно —
  если демон проецирует `claudex-work` именно так, idle-ветка будет вся в Hold
  (fail-closed), пока это не решено отдельно.
- Асимметрия проверок песочницы: idle-ветка требует `readOnly`/`workspaceWrite`,
  а холодный resume отвергает только `dangerFullAccess` — `externalSandbox` и
  пустой `sandbox.type` там проходят. Ветку resume по поручению 840c0d1a не
  меняли; выровнять (`safeSandbox` на обоих путях) — отдельное решение, после
  живой проверки, что resume с `claudex-work` отдаёт.
- Окно гонки: между уведомлением и `queue/add` профиль может сменить
  приложение (повторный read проверяет только статус). Закрыть нечем —
  `thread/read` прав не показывает.
- Липкость: после пробуждения тред остаётся на `claudex-work` — и **открытая
  в приложении сессия тоже**: update меняет права живой сессии на следующие
  ходы, в том числе ходы человека.
- Холодный resume использует `claudex-work`: локальные проекты и серверные
  файлы доступны только для чтения, служебные каталоги Codex/ClauDex и tmp —
  для записи, сеть включена. Важно: сетевой профиль сам по себе не делает SSH
  read-only; удалённая запись требует отдельной read-only учётки/ключа.
- `--full-report` кладёт в тред текст исполнителя целиком.
- Запись, положенную `codex queue` и вычитанную до прихода claudex, нельзя
  связать с ходом: стадия остаётся `queue_accepted` (`queue_item_gone`).
- Предпроверка `deliverThread` для не-live треда ждёт 2×Poll (было и раньше).

## Тесты `internal/task` под fail-closed (поручение d325a41f)

Production не менялся. Успешная доставка в тестах — только на выгруженном
треде без замка писателя (`unloadedHome`): `notLoaded` → resume с
`claudex-work`. `codexHome(t, live...)` даёт loaded idle с замком — с
поручения 840c0d1a это снова успешный путь (через `thread/settings/update`).
В `TestBusyThread…` первая
стадия теперь `not_queued` (отказ до `queue/add`), а сама очередь при отказе
не трогается. Фальшивый app-server после хода держит тред загруженным, поэтому
`TestFlushIsBounded…` даёт каждому поручению свой тред. Названия тестов с
«Live»/«Reopened» оставлены, хотя сценарий в них теперь — выгруженный тред.

## Idle-ветка через `thread/settings/update` (поручение 840c0d1a)

Клиент (`appserver.go`): `SetPermissions` шлёт update и читает сокет до
`thread/settings/updated` своего треда; `recv` запоминает такие уведомления,
даже если они пришли во время другого вызова (последнее перекрывает).
Подделка (`codextest`): update по умолчанию меняет профиль и шлёт
уведомление после ответа; ручки `FailSettings`, `NoSettingsNotice`,
`SettingsNoticeFor`, `SettingsNoticeFirst`, `SettingsSandbox`,
`BusyAfterSettings`, `IgnorePermissions`. Тесты `internal/codex`: успех
(уведомление до и после ответа; порядок `permissionProfile/list` →
`settings/update` → `queue/add`, без resume и `queue/start`), отказы (профиль
запрещён, ошибка update, нет уведомления, уведомление про чужой тред, не тот
профиль, Full Access, `externalSandbox`), тред занят к концу update, своё
сообщение уже в очереди idle-треда (не стартуется явно); busy/approval/
user input/systemError/unknown — ни одного трогающего вызова, включая
`settings/update`. `internal/task`: idle с подтверждением доходит до
`done`; `FailSettings` → Hold `not_queued`. Живые треды не трогались.

## Подтверждение профиля загруженного треда (поручение b0729eba)

Симптом: 3a560c46 (Geo) и 6a1d858b (Payment) — `not_queued` /
`loaded_thread_profile_unconfirmed`: update принят, `thread/settings/updated`
за 10 с не пришло; очередь не тронута. Раньше af63b94d в Payment дошёл — через
холодный resume, после которого тред липко стоит на `claudex-work`.

Корень (исходник `rust-v0.162.1`, `codex-rs/app-server`):

- `bespoke_event_handling.rs`, `EventMsg::ThreadSettingsApplied` → `send_server_notification`
  потокового `ThreadScopedOutgoingMessageSender`, а его `connection_ids` —
  `subscribed_connection_ids(thread)` (`thread_lifecycle.rs`). Подписывают
  `thread/start`/`resume`/`fork`; claudex на загруженный тред их не звал —
  уведомление уходило приложению, не ему. Никакая задержка это не лечит.
- Там же `note_thread_settings` шлёт уведомление, только если настройки
  изменились. Тред уже на `claudex-work` → update пустой → уведомления нет ни
  для кого. Это и есть Payment/Geo после прошлых пробуждений.
- `resume_running_thread`: `permissions` для загруженного треда — всегда
  «mismatch». Если у треда нет подписчиков и он idle — сервер его гасит и
  холодно поднимает уже с переопределением; иначе переопределение
  игнорируется (warn), соединение подписывается, а ответ строится из
  `config_snapshot` — действующие `activePermissionProfile` и `sandbox`.

Живая проверка на одноразовых тредах (`thread/start` в `$CLAUDE_JOB_DIR/tmp`,
`thread/inject_items` вместо хода, после — `thread/archive`; Payment/Geo не
трогались), два соединения: «приложение» (подписано) и «claudex»:

- update от неподписанного → уведомление только приложению; повтор того же
  update → никому; resume открытого треда с `claudex-work` → ответ `:workspace`
  (не применено); update после этого → уведомление и claudex; resume ничейного
  idle треда → `claudex-work` применён.
- `claudex-work` проецируется как `workspaceWrite` (writableRoots `~/.claudex`,
  `~/.codex`, `/private/tmp`, сеть включена) — `safeSandbox` его пропускает.
- Исправленный `Wake` из этой ветки, тред открыт приложением на `:workspace`:
  resume → update → уведомление → `queue/add` → `turn_started` →
  `turn_completed`; повторный `Wake` ход узнал, второй раз не положил. Тред
  открыт и уже на `claudex-work` (как Payment): update не послан, ход начат и
  завершён. Два коротких хода модели на тестовых тредах.

Исправление (`confirmLoaded`): сначала resume, профиль из ответа; update — только
если профиль не `claudex-work`, и уже подписанным соединением. `claudex-work` с
чужой песочницей — `permission_profile_mismatch` без update. Fail-closed
прежний: queue/add только после подтверждения resume или уведомлением.

Подделка (`codextest`) теперь моделирует подписку по соединениям: уведомление —
только подписанному и только при смене; `Watched` — тред открыт другим
клиентом (resume тогда не применяет переопределения; `Held` подразумевает
`Watched`), `SettingsNoticeDelay`, `SettingsProfile`/`NoProfile`. Старый код
на ней падает так же, как в production.

Тесты: `internal/codex` — открытый тред на чужом профиле/Full Access (порядок
`permissionProfile/list` → resume → update → `queue/add`, уведомление до/после
ответа/с задержкой в пределах `Each`); уже на `claudex-work` — без update;
ничейный idle — перезагрузка resume; отказы: ошибка resume/update, нет
уведомления, уведомление про другой тред, позже `Each`, update без эффекта,
не тот/снятый профиль, Full Access/`externalSandbox` из уведомления или из
resume; клиент получает уведомление только после resume. `internal/task` —
открытый тред на `claudex-work` доходит до `done`; повторы наблюдателя
(`Flush`): пока не подтверждено — ни `queue/add`, ни новых записей журнала,
отчёт в `LostReports`; после — одно сообщение, один ход, `done`; потерянное
уведомление подтверждает resume на следующем тике без второго update.

Ограничения: resume подписывает claudex на тред до закрытия соединения (как и
холодный путь); встречные запросы сервера claudex по-прежнему не отвечает.
`thread/resume` пишет в тред `clientInfo` claudex (`set_app_server_client_info`).
Ничейный загруженный тред resume перезагружает — если своё сообщение уже было
в очереди, ход с ним начнётся при resume (под `claudex-work`, как в холодном
пути).

## Следующий шаг

Поручение b0729eba: правки не закоммичены (по условию поручения), бинарь не
ставился. После установки и перезапуска наблюдателя 3a560c46 и 6a1d858b он
должен дожать сам (окно дожима — 72 ч от отчёта, 11.10 ~11:50): их стадия
`not_queued`, сообщение не клалось, дубля не будет. Живой Payment/Geo при этом
получит resume (подписка, переопределение для открытого треда игнорируется) и,
если профиль уже `claudex-work`, — сразу `queue/add`.

Мерж ветки в main (за человеком), `make install`, перезапуск наблюдателя (старый не держит
замок; новый его учитывает и второй не поднимет, пока жив старый pid). Не
проверено вживую: что resume и update с несуществующим профилем отвечают
ошибкой, а не молча берут прежний (гейт — `permissionProfile/list` до них).
