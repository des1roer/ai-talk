# CLAUDE.md

Обзор программы, архитектура и конфигурация — в README (раздел «Для ИИ-ассистентов»): @README.md

Ниже — то, чего в README нет.

## Команды

```sh
go build -ldflags "-H=windowsgui" -o ai-talk.exe .   # основная сборка (без консоли)
go run . -debug                                      # разработка: логи в консоль
go vet ./...
go test ./...
gofmt -l .                                           # должно быть пусто
```

- Окружение: Windows, Go 1.27.1 (`go.mod`). cgo не нужен (в отличие от volume_switch — здесь нет Fyne/GLFW). Bash-инструмент — Git Bash.
- Программа одноэкземплярная (мьютекс `Local\ai_talk_single_instance`): второй запуск молча выходит. Перед проверкой новой сборки закрыть запущенную (иконка в трее → «Выход»), иначе `go build -o ai-talk.exe` упадёт с access denied, пока exe занят.
- Лог — `%LOCALAPPDATA%\ai-talk\ai-talk.log` (`dataDir`); `.env` и `ai-talk.token` ищутся там же, рядом с exe и в рабочем каталоге (`dotEnvPaths`, `tokenFileDirs`).

## Git, PR

- Работа через ветку + PR в `main` (`gh pr create`); мёрджит пользователь. Заголовок PR и коммита — по-английски в повелительном наклонении, описание PR — по-русски.
- CI: `.github/workflows/tests.yml` — `go vet` + `go test -race` на `windows-latest`.

## Соглашения по коду

- Однофайловая программа (`main.go`), по образцу [volume_switch](https://github.com/des1roer/volume_switch) — новые пакеты не заводить. Тесты — в `main_test.go`.
- Win32: сначала готовые функции `golang.org/x/sys/windows`; чего там нет — `LazyProc` в едином блоке `proc…` в начале `main.go`. Ошибку проверять по возвращаемому значению функции и только потом оборачивать `win32Error(name, errno)`.
- Современный Go: `slices`, `errors.AsType`, `errors.Is`.
- Сетевой протокол (токен, `X-Agent-Host`, WebSocket) — общий с [lanagent](https://github.com/des1roer/lanagent); если протокол там меняется, синхронизировать вручную (разных модулей/vendoring между проектами нет).

## Подводные камни

- **Не путать «Running» и «подключено».** `agent.Running()` значит «соединение включено пользователем» (сервер слушает или клиент пытается/держит связь); фактическое состояние (`stateOff`/`stateWaiting`/`stateConnected`) — через `agent.State()` или колбэк `onState`.
- `runServer` принимает только одно соединение за раз (`sessionMu.TryLock()`); второе отклоняется кодом `websocket.StatusPolicyViolation`, а не ставится в очередь — иначе второй компьютер завис бы на подключении без диагностики.
- Останов сервера форсирует закрытие активного WebSocket-соединения (`active.Load().CloseNow()`), потому что `http.Server.Shutdown` не закрывает хайджекнутые (upgrade'нутые) соединения сам.
- **Локальный API не открывать наружу.** Он без токена, поэтому защищён только тем, что слушает `127.0.0.1`, требует `X-AI-Talk: 1` и loopback-`Host` (`apiHandler`). Не убирать эти проверки и не добавлять CORS: иначе сайт в браузере сможет писать второму Claude.
- `/send` проверяет `agent.Running()`: пока соединение выключено в трее, писать нельзя. Скилл не должен уметь включать соединение сам, это решение пользователя.
- Лимит чтения websocket по умолчанию 32 КБ — в `session` он поднят до `maxMessageSize` (1 МБ); ответы Claude бывают длинными.
- Скилл — `skills/ai-talk/SKILL.md`, ставится копированием в `~/.claude/skills/ai-talk/` на обоих ПК. При изменении API обновить скилл и переустановить его.
- `showNotification`/`findOwnTrayWindow` завязаны на внутренности `github.com/getlantern/systray` v1.2.2 (класс окна `SystrayClass`, ID иконки 100) — как и в volume_switch; при обновлении systray проверить, что баллоны ещё показываются.
- В отличие от volume_switch, здесь нет Fyne-окон, поэтому `systray.Run` вызывается прямо из горутины `main()` без отдельной закреплённой горутины и `runtime.LockOSThread()`.

## Тесты

- `agent` тестируется без настоящей сети на другом компьютере: `TestAgentServerStartStop` слушает `127.0.0.1:0` (ОС сама выбирает свободный порт) и сразу останавливается — проверяется только переключение состояний. Обмен сообщениями проверяется парой сервер+клиент в одном процессе (`connectedPair`), API — через `httptest` поверх `apiHandler`.
- Регистр (`tempRegistryKey`) и `dataDir`/`.env` (`setVar`) подменяются так же, как в volume_switch — см. его CLAUDE.md, если нужны детали приёма.
- Не покрыты намеренно: `main`, `trayApp.onReady`/`onExit` (настоящий трей) и ветки Win32-ошибок, которые нельзя вызвать штатно.
