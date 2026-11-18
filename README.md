# ai-talk

*English version: [below](#english).*

Демон для системного трея Windows 11. Включает и выключает соединения по WebSocket с компьютерами в домашней сети и передаёт по ним текстовые сообщения. Состояние (отключено / ожидание / подключено) видно по значку в трее; при подключении и при входящем сообщении всплывает уведомление.

Главное назначение — переписка двух Claude Code на разных компьютерах. Демон отдаёт локальный API на `127.0.0.1`, а скилл [`skills/ai-talk`](skills/ai-talk/SKILL.md) учит Claude через этот API писать собеседнику и ждать ответа. Писать можно, только пока соединение включено в трее.

Протокол и сетевая часть — из [lanagent](https://github.com/des1roer/lanagent): общий секретный токен в заголовке `Authorization`, имя компьютера в `X-Agent-Host`, сторона-сервер слушает порт, сторона-клиент подключается и сама переподключается при обрыве, на `ping` сторона отвечает `pong от <имя>`. Вместо stdin/stdout lanagent сообщения идут через локальный API.

## Возможности

- Какая сторона — сервер (`AGENT_LISTEN`) или клиент (`AGENT_CONNECT`) — решается конфигурацией на каждом компьютере; переключать роль на лету не нужно.
- Пункт меню «Включить/выключить соединение» запускает и останавливает сервер (приём подключений) или клиента (подключение с автоматическими повторами при обрыве) без перезапуска программы.
- Значок в трее и всплывающее уведомление отражают состояние: отключено, ожидание подключения, подключено (с именами собеседников).
- При включении клиент сам переподключается каждые несколько секунд, пока связь не настроена; при разрыве уже установленного соединения — тоже.
- Сервер принимает сколько угодно одновременных подключений (например, второй компьютер и телефон с Android-клиентом сразу) — каждое отдельное отключение не трогает остальные.
- Пункт меню «Показывать входящие сообщения» (с галочкой, по умолчанию включён) включает/выключает всплывающие уведомления о сообщениях. Сообщения при этом всё равно попадают во входящие API. Выбор сохраняется в `HKCU\Software\ai-talk`.
- Пункт меню «Запускать при входе в Windows» (с галочкой) включает/выключает автозапуск через `HKCU\...\Run`.
- Необязательный файл `.env` для конфигурации; если его нет — переменные окружения, затем флаги.
- Соединение включается автоматически при старте программы, если конфигурация задана.
- Локальный API на `127.0.0.1:8767` (только loopback, каждый запрос — с заголовком `X-AI-Talk: 1`):
  - `GET /status` — состояние, список подключённых собеседников (`peers`) и их имена через запятую (`peer`, для обратной совместимости), номер последнего входящего;
  - `POST /send?to=имя` — отправить тело запроса (до 1 МБ) собеседнику с таким именем (`X-Agent-Host`); без `to` — всем, кто сейчас подключён. 409, если соединение выключено в трее или подходящих собеседников нет;
  - `GET /inbox?after=N&wait=15m` — входящие от любого собеседника с номером больше N (поле `from` у каждого сообщения — чьё оно); `wait` держит запрос, пока не придёт новое (не дольше 15 минут). Хранятся последние 500 сообщений, до перезапуска программы.

## Скилл для Claude Code

Скопировать `skills/ai-talk` в `%USERPROFILE%\.claude\skills\ai-talk` на обоих компьютерах. После этого на просьбу вида «напиши второму клоду …» Claude проверяет `/status`, отправляет сообщение через `/send` и ждёт ответа в фоне через `/inbox?wait=`. Если соединение выключено в трее, Claude не включает его сам, а просит пользователя.

## Установка

Готовый `ai-talk.exe` собирается из исходников (см. «Сборка» ниже) — пока без пакетных менеджеров и релизов. Программа переносная: exe можно положить куда угодно.

Лог и необязательный `.env` лежат в `%LOCALAPPDATA%\ai-talk`.

## Сборка

```sh
go build -ldflags "-H=windowsgui" -o ai-talk.exe .   # основная сборка (без консоли)
go run . -debug                                      # разработка: логи в консоль
go vet ./...
go test ./...
```

Без `-H=windowsgui` получится exe с консольной subsystem — Windows будет открывать для него консольное окно при каждом запуске независимо от `-debug` (подробности — в CLAUDE.md volume_switch, откуда взят этот приём).

## Конфигурация

Настройка выполняется для каждого компьютера отдельно: `AGENT_LISTEN` — слушать подключения, `AGENT_CONNECT` — подключаться к другому. Можно задать только один из них (классические роли сервер/клиент) или оба сразу («both») — тогда компьютер одновременно слушает (например, для телефона с Android-клиентом) и держит исходящую связь с другим ПК.

| Флаг       | Переменная окр. | Значение                                                        |
|------------|-----------------|------------------------------------------------------------------|
| `-listen`  | `AGENT_LISTEN`  | Слушать подключения, например `:8080`                            |
| `-connect` | `AGENT_CONNECT` | Подключиться, например `ws://WIN-O3VIUQGMCM8:8080/ws`             |
| `-token`   | `AGENT_TOKEN`   | Общий секрет для обеих сторон                                    |
| —          | `AGENT_API_PORT`| Порт локального API на 127.0.0.1, по умолчанию `8767`            |
| `-debug`   | —               | Показывать консоль с логами отладки                              |

Если заданы и `AGENT_LISTEN`, и `AGENT_CONNECT`, работают оба сразу. Если оба компьютера в паре настроены так (оба слушают и оба пытаются дозвониться друг до друга — симметричная конфигурация для отказоустойчивости: неважно, какой из двух сейчас включён), дозваниваются оба, но лишнее из двух получившихся соединений само закрывается: остаётся то, что набрал компьютер с именем, меньшим по алфавиту. Телефон/планшет при этом остаётся чистым клиентом (`AGENT_CONNECT` без `AGENT_LISTEN`) — ему это правило не касается.

Токен, если не задан флагом/переменной/`.env`, ищется в файле `ai-talk.token` рядом с exe или в `%LOCALAPPDATA%\ai-talk\ai-talk.token` (одна строка с секретом).

Файл `.env` (`KEY=VALUE` построчно, комментарии через `#`) ищется в рабочем каталоге, рядом с exe и в `%LOCALAPPDATA%\ai-talk`; не переопределяет переменную, уже заданную в окружении. Пример:

```
AGENT_LISTEN=:8080
AGENT_TOKEN=supersecret
```

## Лицензия

[MIT](LICENSE).

## Требования

- Go 1.27.1+ (как в `go.mod`)
- Windows (использует `golang.org/x/sys/windows` и прямые вызовы WinAPI; не соберётся и не запустится на других платформах)

## Для ИИ-ассистентов (обзор архитектуры)

Однофайловая программа на Go (`main.go`, `package main`), как и [volume_switch](https://github.com/des1roer/volume_switch), на который она ориентируется по стилю кода, логированию, `.env` и автозапуску.

**Объекты:**

- `agent` — включение/выключение соединений: `Start`/`Stop` запускают/останавливают `runServer` (accept, сколько угодно одновременных подключений), если задан `AGENT_LISTEN`, и/или `runClient` (dial с повторами), если задан `AGENT_CONNECT`, — при обоих сразу (`cfg.mode == "both"`) работают оба параллельно; `session` обслуживает одно установленное соединение (периодический ping, ответ на `ping`, входящие — в `inbox`) — их может быть несколько параллельно; `conns` хранит все текущие (conn → кто это и кем набран), `addPeer`/`removePeer` пересчитывают агрегатное состояние — если от того же имени пришло сразу два соединения (оба компьютера симметричной пары одновременно дозвонились друг до друга), `addPeer` оставляет то, что набрал участник с именем, меньшим по алфавиту, второе закрывает; `Send` пишет одному собеседнику по имени (`to`) или рассылает всем;
- `inbox` — последние входящие с возрастающими ID и ожиданием новых (`wait`); `apiHandler`/`serveAPI` — локальный API для скилла;
- `trayApp` — связывает меню трея (`onReady`/`onExit`) с `agent` через колбэк `onStateChange`, который обновляет значок/подсказку/пункт статуса и показывает уведомление при подключении;
- `config` — итоговые настройки (`resolveConfig`: флаги > окружение/`.env` > умолчания), `autostart` — автозапуск через `HKCU\...\Run`.

**Протокол** (из lanagent): WebSocket (`github.com/coder/websocket`), токен сравнивается `crypto/subtle.ConstantTimeCompare` в заголовке `Authorization: Bearer <token>`, имя компьютера передаётся в `X-Agent-Host`.

**Ключевые решения:**

- Сообщения передаются через локальный HTTP API, а не через stdin/stdout, как в lanagent: демон живёт в трее без консоли, а Claude удобно вызывать `curl`. API слушает только loopback и требует заголовок `X-AI-Talk: 1` и loopback-`Host`. Иначе любая страница в браузере могла бы через `fetch` отправить второму Claude текст от имени пользователя (CSRF, DNS rebinding).
- `systray` в этой программе не требует отдельной закреплённой горутины под Fyne (в отличие от volume_switch): здесь нет UI-окон, поэтому `systray.Run` вызывается прямо из `main()`.
- Значок генерируется во время выполнения в трёх цветных вариантах (`iconOff`/`iconWaiting`/`iconConnected`) тем же приёмом, что и в volume_switch — PNG, обёрнутый в минимальный `.ico`, без внешнего файла ресурсов.

---

<a id="english"></a>

# ai-talk (English)

A Windows 11 system tray daemon: turns WebSocket connections to computers on a home network on and off and carries text messages over them. The tray icon shows the state (off / waiting / connected); a balloon notification appears on connect and on every incoming message. Its main purpose is letting two Claude Code instances on different computers talk to each other: the daemon serves a local API on `127.0.0.1:8767`, and the [`skills/ai-talk`](skills/ai-talk/SKILL.md) skill teaches Claude to use it. Messaging works only while the connection is enabled in the tray.

The protocol and networking code come from [lanagent](https://github.com/des1roer/lanagent): a shared secret token in the `Authorization` header, the computer name in `X-Agent-Host`, the server side listens on a port, the client side connects and reconnects on its own after a drop. Instead of lanagent's stdin/stdout, messages go through the local API (`GET /status`, `POST /send?to=name`, `GET /inbox?after=N&wait=15m`; loopback only, every request needs the `X-AI-Talk: 1` header).

## Features

- Which side is the server (`AGENT_LISTEN`) or the client (`AGENT_CONNECT`) is decided by configuration on each computer; no need to switch roles at runtime.
- The "Включить/выключить соединение" ("Toggle connection") tray menu item starts and stops the server (accepting connections) or the client (connecting, with automatic retries on drop) without restarting the program.
- The tray icon and a balloon notification reflect the state: off, waiting for a connection, connected (with the peers' names).
- When enabled, the client keeps reconnecting every few seconds until the link is up; the same happens after an established connection drops.
- The server accepts any number of simultaneous connections (e.g. a second computer and a phone running the Android client at the same time) — one dropping doesn't affect the others.
- The "Показывать входящие сообщения" ("Show incoming messages") menu item, with a checkmark (on by default), turns message balloon notifications on and off; messages still reach the API inbox. The choice is saved in `HKCU\Software\ai-talk`.
- The "Запускать при входе в Windows" ("Run at Windows sign-in") menu item, with a checkmark, enables/disables autostart via `HKCU\...\Run`.
- An optional `.env` file for configuration; falls back to environment variables, then flags.
- The connection is turned on automatically at startup if configured.

## Building

```sh
go build -ldflags "-H=windowsgui" -o ai-talk.exe .   # main build — no console
go run . -debug                                      # development: logs to console
```

## Configuration

Each computer is configured separately: `AGENT_LISTEN` listens for connections, `AGENT_CONNECT` connects to another. Set just one for the classic server/client roles, or both at once ("both" mode) — the computer then listens (e.g. for a phone running the Android client) and keeps an outgoing link to another PC at the same time.

| Flag       | Env variable    | Meaning                                                  |
|------------|-----------------|-----------------------------------------------------------|
| `-listen`  | `AGENT_LISTEN`  | Listen for connections, e.g. `:8080`                       |
| `-connect` | `AGENT_CONNECT` | Connect to, e.g. `ws://WIN-O3VIUQGMCM8:8080/ws`             |
| `-token`   | `AGENT_TOKEN`   | Shared secret for both sides                               |
| —          | `AGENT_API_PORT`| Local API port on 127.0.0.1, default `8767`                |
| `-debug`   | —               | Show a console with debug logs                             |

If both `AGENT_LISTEN` and `AGENT_CONNECT` are set, both run. If the two computers in a pair are both set up this way (both listening and both trying to dial each other — a symmetric setup for resilience: either one can be the one that's on), both dial out, but the redundant one of the two resulting connections closes itself: the one whose hostname sorts first alphabetically wins. A phone/tablet stays a plain client (`AGENT_CONNECT` without `AGENT_LISTEN`) and isn't affected by this rule.

## License

[MIT](LICENSE).
