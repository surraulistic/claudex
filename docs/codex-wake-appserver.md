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
- Запись, положенную `codex queue` и вычитанную до прихода claudex, связывается
  с ходом по отпечатку текста (`TextHash` из журнала против `userMessage.content`
  в `thread/turns/list` с `itemsView: full`; по схеме 0.163.0 текст есть только
  в `full`, вживую не сверялось). Нет точного совпадения — `queue_accepted` +
  `consumed_unconfirmed`, состояние `unconfirmed`, не дожимается (R7 ревью
  b43800a).
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

## Исправления по ревью 2026-10-17 (поручение 129d623e)

Отчёт: `~/.claudex/reviews/2026-10-17-opus-high-fixes.md`. Коротко:
наблюдатель считает разбуженным только начатый ход и откладывает остальное;
`failed`/`interrupted` — стадия `turn_failed`, не доставка и не повтор; Deadline
действует на тред и разговор; свой ход ищется постранично до первой попытки;
смена прав открытой сессии видна и не повторяется после возврата
(`permission_profile_reverted`); запись `codex queue` узнаётся по sha256 текста.
Происхождение бинаря — `claudex version` (штамп Go во вложенном worktree врёт).
Схема установленного 0.163.0 содержит все используемые поля; вживую 0.163.0 не
проверялся.

## Маршрут по владельцу треда (поручение 0124064d)

База — `origin/main` 167e1e0 (ветка `codex-wake-appserver` в него уже влита),
ветка `worktree-codex-owner-routing`. Не запушено, не установлено.

Симптом: при открытом ChatGPT Desktop отчёты в его треды уходили в
`not_queued` / `thread_open_elsewhere` (последние — 08.11 13:31 и 14:01).

Диагностика (только чтение, Codex 0.167.0, Desktop 26.1103, его CodexCLI
0.167.0-alpha.3.2):

- Два сокета в `/private/tmp/codex-daemon-501/` — не два app-server. `e2a5…` —
  managed daemon (`codex app-server --remote-control --listen unix://
  --managed-daemon`, pid в `~/.codex/app-server-daemon/app-server.pid`), на него
  ведёт `app-server-control.sock`. `d71a…` — `app-server-updater.sock` процесса
  `codex app-server daemon pid-update-loop`; замков тредов он не держит.
- Desktop поднимает свой runtime так (`app.asar`, `.vite/build`):
  `codex -c features.code_mode_host=true app-server --analytics-default-enabled`
  — транспорт по умолчанию stdio, сокета нет. `~/.codex/ipc/ipc.sock` — приватный
  IPC Electron (кадры с длиной + JSON), не API Codex; не используется.
- Лог демона за 12:02–13:05Z: `thread-store conflict: thread … already has an
  active writer` — демон сам не может поднять тред, который держит Desktop.
- Исходник `rust-v0.167.0`: `ext/queue/src/service.rs`,
  `watch_external_messages` — каждый app-server раз в 10 с сверяет
  `change_version` общей очереди и для загруженных у себя свободных тредов
  стартует ход (`start_turn_if_idle`, `turn_trigger: "queue"`) с настройками
  треда. Строки этого кода есть в обоих бинарях. `thread/queue/add` у
  незагруженного треда только пишет в очередь (`wake_if_loaded` — ничего).
  `codex queue` — тот же `thread/queue/add` через демон или встроенный сервер.
- `thread_processor.rs`, `normalize_thread_turns_status`: читатель, у которого
  тред не загружен, переписывает `inProgress` в `interrupted`. Вживую: пока
  владелец видел `inProgress`, демон отдавал `interrupted`, `completedAt=0`;
  через 3 с оба — `completed`. У настоящего обрыва (`TurnAborted`) `completedAt`
  есть.

Решение: замок есть, демон говорит `notLoaded` → `codex.ThreadOwner` (lsof по
файлу замка, ps по держателю). Другой `codex app-server` без подкоманды →
`thread/queue/add` через демон с `QueueOnlyText` (событие, не отчёт), без
`permissionProfile/list`, resume, `settings/update`, `queue/start`; ход ищется в
общей истории по `clientUserMessageId`. Замок у самого демона (pid за сокетом —
`LOCAL_PEERPID`/`SO_PEERCRED`) или pid неизвестен, держателя нет, их несколько,
lsof/ps не ответили → `thread_owner_unknown`. Не app-server → прежний
`thread_open_elsewhere`. `interrupted` без `completedAt` — начатый ход.
Выключатель — `CLAUDEX_CODEX_OWNER_QUEUE=off`.

Живой smoke (`go test -tags codexlive -run TestLive ./internal/codex`,
`CLAUDEX_SMOKE_DIR` — пустой каталог; только одноразовые треды, после —
`thread/archive`; рабочие треды не трогались):

- Владелец — та же команда, что у Desktop, тем же CodexCLI из ChatGPT.app
  (Desktop при этом закрыт). `ThreadOwner` → `app_server`, «ChatGPT Desktop».
  `Wake` через демон: `queue_accepted` → `turn_started` (`owner_queue`) за
  7.2 с → `turn_completed`. Демон тред не загружал (`notLoaded` до конца).
- Тред, загруженный в демоне (режим remote-control): `resume_loaded` с
  `claudex-work` → `turn_started` → `turn_completed`.

Ограничения:

- Ход в треде Desktop идёт с правами, которые там выставил человек (в
  `.codex-global-state.json` у Desktop `agent-mode: full-access`). Сменить их
  снаружи нечем: у Desktop нет внешнего endpoint, а демон тред не держит.
  Поэтому туда идёт только событие, а `CLAUDEX_CODEX_OWNER_QUEUE=off`
  возвращает прежний отказ.
- Ход начнётся, только когда тред у владельца свободен: занят, ждёт одобрения,
  Desktop закрыли — `queue_accepted`/`owner_turn_pending`, пока хода не видно.
  Закрыли Desktop — тред освобождается, и следующий дожим идёт холодным resume
  в демоне под `claudex-work`; своё сообщение он находит в очереди и второй раз
  не кладёт.
- Живой Desktop с этим кодом не проверялся (на время работы он был закрыт);
  проверена его команда и его бинарь. Доставка через установленный `claudex`
  тоже не проверена: бинарь не ставился.
- Настоящий обрыв без `completedAt` (старые rollout) будет считаться начатым,
  а не `turn_failed`.
- Сообщение, которое человек удалил из очереди Desktop, неотличимо от
  забранного владельцем: `owner_turn_pending` до конца окна дожима (72 ч).
- Владелец определяется через `/usr/sbin/lsof` и `/bin/ps` (≈0.3 с на
  попытку); на Linux pid за сокетом — `SO_PEERCRED`, на прочих ОС он
  неизвестен, и маршрут владельца там всегда `thread_owner_unknown`.

Следующий шаг: ревью, мерж, `make install`, перезапуск наблюдателя; затем с
открытым Desktop — `claudex send` в одноразовый тред Desktop и
`claudex task log <id>`: ждать `owner_queue` → `turn_started`.

## Ревью маршрута владельца 2026-11-12 (поручение 413b9084)

Кандидат `2ca645e` (от `origin/main` 167e1e0) принят: блокеров нет, влит в
локальный `main` fast-forward вместе с довесками ниже. Не запушено, не
установлено; установленный `claudex` — ревизия 167e1e0, без маршрута владельца.

### Что проверено

- Сборка, `go vet`, `go test ./...` (go1.26.1) — зелёные; `gofmt` чист;
  `GOOS=linux` собирается. `GOOS=windows` не собирается из-за `syscall.Flock`
  в `internal/journal` — это до кандидата, не его.
- Исходники `rust-v0.167.1` (commit 595a35e, через GitHub API), факты:
  - `ext/queue/src/service.rs`, `watch_external_messages`: опрос раз в 10 с
    (`interval(Duration::from_secs(10))`) по `change_version` SQLite;
    `changes_since(last_revision, &manager.list_thread_ids())` — только треды,
    **загруженные в этом процессе**; dispatch → `wake_if_loaded` →
    `on_thread_idle` → `dispatch_if_idle` → `start_turn_if_idle`
    (`turn_trigger: "queue"`). `enqueue` для незагруженного треда — запись в
    очередь и ничего больше (`wake_if_loaded` не находит тред).
  - Очередь — `LocalQueueStore` поверх `state_db` (SQLite в `$CODEX_HOME`),
    общая для всех app-server с тем же домом; создаётся только при
    `experimental_thread_store = local` (`message_processor.rs`). Строки этого
    кода есть в обоих бинарях — демона 0.167.1 и CodexCLI Desktop
    0.167.0-alpha.3.2.
  - `rollout/src/writer_lock.rs`: замок — `try_lock` на открытом
    `thread-writer-locks/<id>.lock`, файл держится открытым до `Drop`, поэтому
    `lsof` видит держателя. Чужой замок — `WouldBlock`, «thread … already has
    an active writer». Осиротевший замок (файл без держателя) снимает первый
    же `acquire` любого процесса Codex.
  - `thread_processor.rs`, `normalize_thread_turns_status`: у читателя, для
    которого тред не Active, `inProgress` переписывается в `interrupted`;
    `completedAt` в `Turn` v2 есть (`completed_at: Option<i64>`).
  - TUI: `daemon_auto_start` — `Stage::Stable`, `default_enabled: true`
    (`features/src/lib.rs`); `startup_orchestration.rs` поднимает managed
    daemon и идёт через него, если нет `--no-daemon`. Значит обычный
    `codex`/`codex resume` держит тред **в демоне**, а не сам.
  - Документация (learn.chatgpt.com/docs/app-server): после ухода последнего
    подписчика тред выгружается через 60 с (`thread_unload_delay_secs`).
    `codex queue --resume` — официальный «положить и поднять на локальном
    демоне»; без `--resume` документация не обещает старта хода.
- Живой срез 12.11 00:44: все четыре замка — у демона pid 89328
  (`app-server --remote-control --listen unix:// --managed-daemon`), включая
  тред координатора; app-server Desktop pid 6362 — `codex app-server --listen
  stdio://` (не `-c features.code_mode_host=true …`, как в тексте коммита) —
  тредов не держал; второй сокет — updater pid 21193. В 00:56 установленный
  `claudex done` поднял тред координатора холодным resume под `claudex-work`:
  телефон ушёл, демон выгрузил тред — правило 60 с видно на рабочем треде.
- Живой smoke (`-tags codexlive`, одноразовые треды, после — `thread/archive`):
  - владелец — отдельный stdio app-server бинарём Desktop (pid 74958):
    `ThreadOwner` → `app_server`, `Wake` через демон 7.6 с → `turn_started`
    (`owner_queue`) → `turn_completed`; демон тред не загружал;
  - тред, загруженный в демоне: `resume_loaded` → `turn_completed`;
  - холодный (новый `TestLiveColdRoute`): после ухода подписчика демон выгрузил
    тред через 61 с, замка нет, `thread/resume` под `claudex-work` →
    `turn_started` → `turn_completed`.
- Не проверено вживую: настоящий тред Desktop (на время ревью Desktop тредов
  не держал, одноразового треда в нём нет) и поведение его app-server в App
  Nap при свёрнутом окне.

### Находки и довески

1. Командная строка Desktop сменилась на `app-server --listen stdio://`;
   `classifyOwner` её и так разбирает (`--listen` — флаг со значением), но
   текст README, комментарий smoke и тесты описывали прежнюю. Исправлено,
   случай добавлен в `TestClassifyOwner`.
2. README относил TUI к «не app-server»: по умолчанию TUI идёт через демон, и
   для claudex такой тред — загруженный в демоне (путь с подпиской и, если
   нужно, сменой прав). Отказ `thread_open_elsewhere` остаётся за `codex
   exec` и `codex --no-daemon`. Исправлено.
3. `claudex done` без доводов закрывал единственное открытое поручение сессии
   пустым «готово» и будил ведущего (воспроизведено на этом поручении).
   Теперь отказ с подсказкой (`MinArgs: 1`), закреплено тестом и зондом.
4. Косметика, оставлено: в ветке «принято, записи нет, владелец app-server»
   `Transport` остаётся `app_server`, журнал новую запись не делает.
5. Осиротевший замок даёт `thread_owner_unknown` — fail-closed и временно:
   следующий `acquire` любого Codex его снимет. Оставлено.

### Матрица состояний

Codex 0.167.1 (демон), Desktop CodexCLI 0.167.0-alpha.3.2. «Факт» —
проверено вживую или по исходнику; «вывод» — следует из фактов, вживую не
снято.

| состояние | кто держит тред | интерфейс claudex | кто просыпается | основание |
|---|---|---|---|---|
| выгружен, замка нет; демон жив (Desktop закрыт, телефон ушёл > 60 с, TUI закрыт) | никто | `thread/resume` с `claudex-work` → `queue/add` (→ `queue/start` при нужде) через демон | демон, права `claudex-work` (липко) | факт: smoke cold; координатор 00:56 |
| загружен в демоне, idle, есть подписчик (телефон/Desktop по remote-control, обычный TUI) | демон | `resume` (подписка) → при нужде `settings/update` → `queue/add` | демон сам (`enqueue` → `wake_if_loaded`) | факт: smoke daemon |
| загружен в демоне, active / ждёт человека | демон | ничего, `thread_busy` / `thread_waiting_on_human`; дожим | после хода демон вычитает очередь сам | факт: 00:56:57 на координаторе; `dispatch_if_idle` |
| в app-server Desktop, Desktop открыт, тред idle | Desktop (stdio) | `queue/add` через демон в общий SQLite, событие | Desktop ≤ 10 с (`watch_external_messages`), права Desktop | факт на суррогате (тот же бинарь): 7.6 с; живой Desktop — не снято |
| в Desktop, окно свёрнуто / в фоне | Desktop | то же | то же, пока процесс не усыплён | вывод; App Nap не проверялся |
| в Desktop, тред занят / ждёт одобрения | Desktop | `queue_accepted` + `owner_turn_pending`, дожим ищет ход | Desktop, когда освободится | факт по исходнику (`start_turn_if_idle`) |
| Desktop закрыт или тред в нём закрыт | через 60 с без подписчиков / сразу при выходе — никто | → строка 1 | демон | факт: 60 с (док, smoke cold) |
| демона нет (remote-control не запущен, managed daemon не поднят) | — | `codex queue` (durable, без пробуждения) | никто до `codex queue --resume`, открытия треда человеком или появления демона (дожим) | факт по исходнику (`enqueue` без загруженного треда) и CLI-док |
| держит `codex exec` / `codex --no-daemon` | тот процесс | `thread_open_elsewhere`, ничего | никто | факт: отказ по коду; встроенный сервер TUI с state_db очередь тоже опрашивал бы, но от `exec` по `ps` не отличим — оставлен отказ |
| замок без процесса / два держателя / `lsof` нет | неясно | `thread_owner_unknown`, ничего | никто; осиротевший замок снимет следующий `acquire` | факт по исходнику |
| телефон подключён, тред в демоне | демон (телефон — подписчик) | строки 2–3 | демон | факт: 00:30 замок у демона |
| телефон отключён | через 60 с — никто | строка 1 | демон | факт: 00:56 cold resume координатора |
| Mac спит / приложение закрыто | — | очередь durable | после пробуждения — строки 1–2 | док («remote access stops until available») |

Ответы на вопросы поручения: `thread/queue/add` для незагруженного треда —
**только durable enqueue без пробуждения** (факт). Закрытый в UI или
завершивший ход тред **автоматически возобновляется без второго писателя
ровно тогда, когда замка нет** — холодным `resume` в демоне; при чужом замке
единственный законный путь — очередь, ход начинает владелец.

### Минимальная архитектура (= кандидат + наблюдатель)

1. Замок читается (`lsof`/`ps`), не берётся: держатель выбирает маршрут.
2. Замка нет → `resume` под `claudex-work` в демоне (claudex — единственный
   писатель), `queue/add`, опрос хода.
3. Замок у `codex app-server` (Desktop, IDE) → только `queue/add` через API
   демона (общий SQLite, но не напрямую), событие, не отчёт; ход — владельца.
4. Замок у не-app-server или не установлен → отказ, ничего не кладём.
5. Демона нет → `codex queue` (durable); наблюдатель поднимет холодным
   `resume`, когда демон появится.
6. Наблюдатель: повтор по `clientUserMessageId` без дублей, `FollowTurns`,
   окно 72 ч.

Не нужно и не делается: `codex exec resume` (новый процесс, второй писатель,
дорого), прямая запись в SQLite, снятие замков, подмена владельца.

### Невозможные гарантии и внешние ограничения

- Ход у чужого владельца идёт с его правами (у Desktop — какие выставил
  человек), сменить их снаружи нечем.
- Нет демона и тред закрыт → никто не проснётся до человека или демона.
- Усыплённый Desktop (App Nap) или спящий Mac — пауза до пробуждения.
- Холодный `resume` липко переводит тред на `claudex-work`, в том числе для
  последующих ходов человека с телефона.
- Удалённая человеком из очереди Desktop запись неотличима от забранной:
  `owner_turn_pending` до конца окна дожима.
- Старые rollout с `interrupted` без `completedAt` считаются начатыми.
- Linux — `SO_PEERCRED`; прочие ОС — маршрут владельца всегда
  `thread_owner_unknown`.

Следующий шаг (за человеком): push `main`, `make install`, перезапуск
наблюдателя; затем с открытым Desktop — `claudex send` в одноразовый тред
Desktop, ждать `owner_queue` → `turn_started`.
