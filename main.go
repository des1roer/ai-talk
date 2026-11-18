// Command ai-talk сидит в системном трее и держит (или не держит —
// переключателем в меню) соединения с компьютерами в домашней сети поверх
// WebSocket, используя тот же протокол, что и lanagent:
// общий секретный токен в заголовке Authorization и заголовок
// X-Agent-Host с именем компьютера.
//
// Какая сторона — сервер или клиент — решает конфигурация:
//
//	AGENT_LISTEN=:8080                          слушать подключения
//	AGENT_CONNECT=ws://WIN-O3VIUQGMCM8:8080/ws  подключаться к другому компьютеру
//	AGENT_TOKEN=...                             общий секрет
//
// Пункт меню в трее включает и выключает соединение; значок и всплывающие
// уведомления показывают состояние (отключено / ожидание / подключено) и
// входящие сообщения. Отправлять и читать сообщения можно через локальный
// API на 127.0.0.1 (см. apiHandler) — им пользуется скилл Claude, чтобы
// переписываться с Claude на другом компьютере, пока соединение включено.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/coder/websocket"
	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	appDirName    = "ai-talk" // каталог программы в %LOCALAPPDATA%: лог и .env
	logFileName   = "ai-talk.log"
	tokenFileName = "ai-talk.token"

	pingInterval   = 15 * time.Second // протокольный ping, чтобы быстрее заметить обрыв
	dialTimeout    = 10 * time.Second
	dialRetryDelay = 3 * time.Second // пауза между неудачными попытками подключиться
	reconnectDelay = time.Second     // пауза перед повторным подключением после разрыва

	defaultAPIPort = "8767"           // локальный API: 127.0.0.1:AGENT_API_PORT
	apiHeader      = "X-AI-Talk"      // обязательный заголовок запросов к API
	maxMessageSize = 1 << 20          // предел одного сообщения (у websocket по умолчанию 32 КБ)
	maxWait        = 15 * time.Minute // предел ожидания в /inbox?wait=
	inboxCap       = 500              // сколько последних входящих хранить
)

// version — версия программы; релизная сборка подставляет её из тега через
// -ldflags "-X main.version=1.2.3".
var version = "dev"

var host, _ = os.Hostname()

// dataDir возвращает каталог для лога и .env — %LOCALAPPDATA%\ai-talk. Сам
// каталог не создаёт. Рабочий каталог для этого не годится: при автозапуске
// это System32, а при установке через пакетный менеджер — каталог пакета.
// Переменная только ради тестов.
var dataDir = func() (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDirName), nil
}

// win32Error оборачивает errno, который LazyProc.Call отдаёт третьим
// значением. Он осмыслен только когда сама функция сообщила о неудаче своим
// возвращаемым значением (обычно 0), поэтому вызывать win32Error нужно только
// после такой проверки.
func win32Error(name string, errno error) error {
	if e, ok := errors.AsType[syscall.Errno](errno); ok && e == 0 {
		return fmt.Errorf("%s: неизвестная ошибка", name)
	}
	return fmt.Errorf("%s: %w", name, errno)
}

// Win32-функции, которых нет готовыми в golang.org/x/sys/windows.
var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procAllocConsole     = kernel32.NewProc("AllocConsole")
	procFreeConsole      = kernel32.NewProc("FreeConsole")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
)

// user32 нужен только для ShowWindow (возврат консоли на экран в -debug).
var (
	user32         = windows.NewLazySystemDLL("user32.dll")
	procShowWindow = user32.NewProc("ShowWindow")
)

// ---------------------------------------------------------------------------
// Логирование
// ---------------------------------------------------------------------------

// ensureConsole делает консоль видимой.
//   - Если консоли нет (сборка с -H windowsgui) — создаёт её через AllocConsole.
//   - Если консоль есть, но скрыта библиотекой systray — показывает обратно.
func ensureConsole() error {
	const swShow = 5

	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		if r, _, errno := procAllocConsole.Call(); r == 0 {
			return win32Error("AllocConsole", errno)
		}
		hwnd, _, _ = procGetConsoleWindow.Call()
		if hwnd == 0 {
			return errors.New("GetConsoleWindow: консоль создана, но окна у неё нет")
		}
		out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("открытие CONOUT$: %w", err)
		}
		os.Stdout = out
		os.Stderr = out
		log.SetOutput(out)
		in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("открытие CONIN$: %w", err)
		}
		os.Stdin = in
	}
	_, _, _ = procShowWindow.Call(hwnd, swShow)
	return nil
}

// setupLogging настраивает вывод: в консоль (debug) или в файл. Возвращает
// функцию, закрывающую лог-файл; вызывается из main при завершении.
func setupLogging(debug bool) (closeLog func()) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if debug {
		consoleErr := ensureConsole()
		log.SetOutput(os.Stderr)
		log.Printf("=== ai-talk %s starting (debug mode) ===", version)
		if consoleErr != nil {
			log.Printf("console: %v", consoleErr)
		}
		return func() {}
	}
	consoleErr := hideConsole()
	path, f, err := openLogFile()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ai-talk: не удалось открыть лог, лог отключён: %v\n", err)
		if consoleErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "ai-talk: console: %v\n", consoleErr)
		}
		log.SetOutput(io.Discard)
		return func() {}
	}
	log.SetOutput(f)
	log.Printf("=== ai-talk %s starting (log -> %s) ===", version, path)
	if consoleErr != nil {
		log.Printf("console: %v", consoleErr)
	}
	return func() {
		log.SetOutput(io.Discard)
		if err := f.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "ai-talk: закрытие %s: %v\n", path, err)
		}
	}
}

// openLogFile открывает на дозапись лог в dataDir, создавая каталог при
// необходимости, и возвращает путь к нему.
func openLogFile() (string, *os.File, error) {
	dir, err := dataDir()
	if err != nil {
		return "", nil, fmt.Errorf("каталог данных: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, logFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	return path, f, err
}

// hideConsole отвязывает процесс от унаследованной консоли и закрывает её
// окно — так же, как в volume_switch (см. его CLAUDE.md про -H=windowsgui).
func hideConsole() error {
	r, _, errno := procFreeConsole.Call()
	if r == 0 && !errors.Is(errno, windows.ERROR_INVALID_PARAMETER) {
		return win32Error("FreeConsole", errno)
	}
	return nil
}

// ---------------------------------------------------------------------------
// .env
// ---------------------------------------------------------------------------

// dotEnvPaths возвращает, где искать .env, в порядке приоритета: рабочий
// каталог (разработка, `go run .`), каталог exe (переносная установка) и
// dataDir. Повторяющиеся каталоги отбрасываются.
func dotEnvPaths() []string {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if dir, err := dataDir(); err == nil {
		dirs = append(dirs, dir)
	}
	var paths []string
	for _, dir := range dirs {
		path := filepath.Join(dir, ".env")
		if !slices.ContainsFunc(paths, func(p string) bool { return strings.EqualFold(p, path) }) {
			paths = append(paths, path)
		}
	}
	return paths
}

// loadDotEnv читает KEY=VALUE построчно из .env и выставляет переменные
// окружения, не перезаписывая уже заданные извне (в том числе загруженные из
// .env с более высоким приоритетом).
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf(".env: %s не загружен: %v", path, err)
		return
	}
	loaded := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				log.Printf(".env: не удалось задать %s: %v", key, err)
				continue
			}
			loaded++
			log.Printf(".env: %s=%s", key, value)
		} else {
			log.Printf(".env: %s уже задан извне, пропускаю", key)
		}
	}
	log.Printf(".env: %s загружен, %d переменных", path, loaded)
}

// ---------------------------------------------------------------------------
// Конфигурация
// ---------------------------------------------------------------------------

// config — итоговые настройки программы: какая сторона соединения (сервер
// или клиент) и на что ориентироваться.
type config struct {
	debug bool

	mode       string // "server", "client" или "" — не настроено
	listenAddr string // AGENT_LISTEN, например :8080
	connectURL string // AGENT_CONNECT, например ws://WIN-O3VIUQGMCM8:8080/ws
	token      string
	apiAddr    string // 127.0.0.1:AGENT_API_PORT — локальный API для скилла
}

// resolveConfig собирает настройки по приоритету: флаги, затем окружение (к
// этому моменту в нём уже и значения из .env). Пустой флаг означает «не
// задан». Если заданы и AGENT_LISTEN, и AGENT_CONNECT, побеждает режим
// сервера.
func resolveConfig(debug bool, listenFlag, connectFlag, tokenFlag string) config {
	cfg := config{debug: debug}
	cfg.listenAddr = os.Getenv("AGENT_LISTEN")
	cfg.connectURL = os.Getenv("AGENT_CONNECT")
	if listenFlag != "" {
		cfg.listenAddr = listenFlag
	}
	if connectFlag != "" {
		cfg.connectURL = connectFlag
	}
	cfg.token = resolveToken(tokenFlag)
	port := os.Getenv("AGENT_API_PORT")
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		if port != "" {
			log.Printf("config: AGENT_API_PORT=%q не порт, беру %s", port, defaultAPIPort)
		}
		port = defaultAPIPort
	}
	cfg.apiAddr = net.JoinHostPort("127.0.0.1", port)

	switch {
	case cfg.listenAddr != "" && cfg.connectURL != "":
		log.Printf("config: заданы и AGENT_LISTEN, и AGENT_CONNECT — используется режим сервера")
		cfg.mode = "server"
	case cfg.listenAddr != "":
		cfg.mode = "server"
	case cfg.connectURL != "":
		cfg.mode = "client"
	}
	return cfg
}

// resolveToken возвращает общий секрет: флаг, иначе переменная окружения
// AGENT_TOKEN (включая значение из .env), иначе файл ai-talk.token рядом с
// exe или в dataDir.
func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("AGENT_TOKEN"); v != "" {
		return v
	}
	for _, dir := range tokenFileDirs() {
		if b, err := os.ReadFile(filepath.Join(dir, tokenFileName)); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func tokenFileDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if dir, err := dataDir(); err == nil {
		dirs = append(dirs, dir)
	}
	return dirs
}

// configured сообщает, достаточно ли настроек, чтобы вообще пытаться
// соединяться.
func (c config) configured() bool {
	return c.mode != "" && c.token != ""
}

// ---------------------------------------------------------------------------
// Состояние соединения
// ---------------------------------------------------------------------------

type connState int

const (
	stateOff connState = iota
	stateWaiting
	stateConnected
)

func (s connState) String() string {
	switch s {
	case stateWaiting:
		return "waiting"
	case stateConnected:
		return "connected"
	default:
		return "off"
	}
}

// errNotConnected — Send, когда соединение ещё не установлено.
var errNotConnected = errors.New("собеседник не подключён")

// ---------------------------------------------------------------------------
// agent — соединение между компьютерами и обмен сообщениями
//
// Протокол — как у lanagent: WebSocket, токен в Authorization, имя компьютера
// в X-Agent-Host, текстовые кадры; на "ping" сторона сама отвечает
// "pong от <имя>". Start запускает сервер (accept) или клиент (dial с
// повторами) по cfg.mode, Stop останавливает. Входящие сообщения
// складываются в inbox, откуда их забирает локальный API (apiHandler).
// ---------------------------------------------------------------------------

type agent struct {
	cfg   config
	inbox *inbox

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	state  connState
	peer   string                     // имена подключённых собеседников через ", "
	conns  map[*websocket.Conn]string // установленные соединения: conn -> имя собеседника

	// onState вызывается при каждом изменении агрегатного состояния,
	// onPeerConnected/onPeerDisconnected — при подключении/отключении
	// конкретного собеседника (в т.ч. когда он не единственный), onMessage —
	// на каждое входящее сообщение (кроме ping). Задаются до Start.
	onState            func(state connState, peer string)
	onMessage          func(m message)
	onPeerConnected    func(peer string)
	onPeerDisconnected func(peer string)
}

func newAgent(cfg config, onState func(connState, string)) *agent {
	return &agent{cfg: cfg, inbox: newInbox(inboxCap), onState: onState}
}

// Running сообщает, включено ли соединение в трее (не то же самое, что
// «подключено прямо сейчас» — см. State).
func (a *agent) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancel != nil
}

// State возвращает текущее состояние и, если есть, имя собеседника.
func (a *agent) State() (connState, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state, a.peer
}

// Start запускает сервер или клиент согласно cfg.mode. Повторный вызов,
// пока соединение уже запущено, ничего не делает.
func (a *agent) Start() error {
	if !a.cfg.configured() {
		return errors.New("не настроено: задайте AGENT_LISTEN или AGENT_CONNECT, и AGENT_TOKEN")
	}

	a.mu.Lock()
	if a.cancel != nil {
		a.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.cancel = cancel
	a.done = done
	a.mu.Unlock()

	a.setState(stateWaiting, "")
	go func() {
		defer close(done)
		switch a.cfg.mode {
		case "server":
			a.runServer(ctx)
		case "client":
			a.runClient(ctx)
		}
		if ctx.Err() == nil {
			// Завершилось само (например, порт занят), а не через Stop().
			a.mu.Lock()
			a.cancel = nil
			a.done = nil
			a.mu.Unlock()
			a.forceOff()
		}
	}()
	return nil
}

// Stop останавливает соединение и ждёт, пока фоновая горутина завершится.
func (a *agent) Stop() {
	a.mu.Lock()
	cancel := a.cancel
	done := a.done
	a.cancel = nil
	a.done = nil
	a.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	a.forceOff()
}

func (a *agent) setState(s connState, peer string) {
	a.mu.Lock()
	a.state = s
	a.peer = peer
	a.mu.Unlock()
	if a.onState != nil {
		a.onState(s, peer)
	}
}

// forceOff сбрасывает состояние в "выключено" и разрывает учёт всех
// соединений — используется там, где агрегатное состояние должно стать
// определённым независимо от того, что происходит в уже завершающихся
// session (Stop, самопроизвольное завершение runServer/runClient).
func (a *agent) forceOff() {
	a.mu.Lock()
	a.conns = nil
	a.state = stateOff
	a.peer = ""
	a.mu.Unlock()
	if a.onState != nil {
		a.onState(stateOff, "")
	}
}

// addPeer регистрирует установленное соединение c с собеседником name,
// пересчитывает агрегатное состояние/список имён и уведомляет onState и
// onPeerConnected.
func (a *agent) addPeer(c *websocket.Conn, name string) {
	a.mu.Lock()
	if a.conns == nil {
		a.conns = make(map[*websocket.Conn]string)
	}
	a.conns[c] = name
	joined := a.joinedPeersLocked()
	a.state = stateConnected
	a.peer = joined
	a.mu.Unlock()
	if a.onState != nil {
		a.onState(stateConnected, joined)
	}
	if a.onPeerConnected != nil {
		a.onPeerConnected(name)
	}
}

// removePeer снимает соединение c с учёта и пересчитывает агрегатное
// состояние: connected, если остались другие собеседники, иначе waiting
// (если соединение всё ещё включено) или off.
func (a *agent) removePeer(c *websocket.Conn) {
	a.mu.Lock()
	name := a.conns[c]
	delete(a.conns, c)
	joined := a.joinedPeersLocked()
	var s connState
	switch {
	case joined != "":
		s = stateConnected
	case a.cancel != nil:
		s = stateWaiting
	default:
		s = stateOff
	}
	a.state = s
	a.peer = joined
	a.mu.Unlock()
	if a.onState != nil {
		a.onState(s, joined)
	}
	if name != "" && a.onPeerDisconnected != nil {
		a.onPeerDisconnected(name)
	}
}

// joinedPeersLocked возвращает имена подключённых собеседников через ", " по
// алфавиту. Вызывающий должен держать a.mu.
func (a *agent) joinedPeersLocked() string {
	names := make([]string, 0, len(a.conns))
	for _, n := range a.conns {
		names = append(names, n)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// PeerNames возвращает имена сейчас подключённых собеседников по алфавиту.
func (a *agent) PeerNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.conns))
	for _, n := range a.conns {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Send отправляет text собеседникам по установленным соединениям: если to не
// пусто — только тому, чьё имя (X-Agent-Host) совпадает с to, иначе всем, кто
// сейчас подключён. Возвращает имена тех, кому реально удалось отправить;
// errNotConnected — если подходящих собеседников нет вовсе.
func (a *agent) Send(ctx context.Context, to, text string) ([]string, error) {
	a.mu.Lock()
	targets := make(map[*websocket.Conn]string, len(a.conns))
	for c, name := range a.conns {
		if to == "" || strings.EqualFold(name, to) {
			targets[c] = name
		}
	}
	a.mu.Unlock()
	if len(targets) == 0 {
		return nil, errNotConnected
	}

	wctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var sent []string
	var errs []error
	for c, name := range targets {
		if err := c.Write(wctx, websocket.MessageText, []byte(text)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		sent = append(sent, name)
	}
	if len(sent) == 0 {
		return nil, errors.Join(errs...)
	}
	slices.Sort(sent)
	return sent, nil
}

// runServer слушает cfg.listenAddr и принимает сколько угодно одновременных
// соединений — каждое ведёт свою session в своей горутине (её запускает сам
// net/http на каждый запрос). Останавливается, когда отменят ctx, закрывая
// HTTP-сервер и все установленные соединения.
func (a *agent) runServer(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+a.cfg.token)) != 1 {
			log.Printf("agent: отказ %s: неверный токен", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Agent-Host", host)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		peer := r.Header.Get("X-Agent-Host")
		if peer == "" {
			peer = r.RemoteAddr
		}
		log.Printf("agent: соединение: %s", peer)
		a.session(ctx, c, peer)
		log.Printf("agent: соединение закрыто: %s", peer)
	})

	srv := &http.Server{Addr: a.cfg.listenAddr, Handler: mux}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Printf("agent: %s слушает %s", host, a.cfg.listenAddr)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("agent: сервер остановлен с ошибкой: %v", err)
		}
	case <-ctx.Done():
		// http.Server.Shutdown не закрывает хайджекнутые (upgrade'нутые)
		// соединения сам — закрываем все явно, иначе их session() не
		// заметит отмену ctx.
		a.closeAllConns()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Printf("agent: остановка сервера: %v", err)
		}
		<-errCh
	}
}

// closeAllConns форсирует закрытие всех установленных сейчас соединений.
func (a *agent) closeAllConns() {
	a.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(a.conns))
	for c := range a.conns {
		conns = append(conns, c)
	}
	a.mu.Unlock()
	for _, c := range conns {
		c.CloseNow()
	}
}

// runClient подключается к cfg.connectURL и переподключается при обрыве,
// пока не отменят ctx.
func (a *agent) runClient(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		c, resp, err := websocket.Dial(dctx, a.cfg.connectURL, &websocket.DialOptions{
			HTTPHeader: http.Header{
				"Authorization": {"Bearer " + a.cfg.token},
				"X-Agent-Host":  {host},
			},
		})
		cancel()
		if err != nil {
			log.Printf("agent: нет связи: %v; повтор через %s", err, dialRetryDelay)
			select {
			case <-time.After(dialRetryDelay):
			case <-ctx.Done():
				return
			}
			continue
		}

		// Сервер ai-talk называет себя в X-Agent-Host ответа; lanagent — нет.
		peer := resp.Header.Get("X-Agent-Host")
		if peer == "" {
			peer = a.cfg.connectURL
		}
		log.Printf("agent: подключено к %s (%s)", a.cfg.connectURL, peer)
		a.session(ctx, c, peer)
		log.Printf("agent: соединение потеряно")
		if ctx.Err() != nil {
			return
		}
		select {
		case <-time.After(reconnectDelay):
		case <-ctx.Done():
			return
		}
	}
}

// session обслуживает установленное соединение, пока его не закроет
// собеседник или не отменят ctx: раз в pingInterval шлёт протокольный ping,
// чтобы заметить обрыв (например, ноутбук уснул), отвечает на "ping" и
// складывает остальные входящие сообщения в inbox. Несколько session могут
// выполняться одновременно (сервер с несколькими подключёнными пирами).
func (a *agent) session(ctx context.Context, c *websocket.Conn, peer string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.CloseNow()
	c.SetReadLimit(maxMessageSize)

	a.addPeer(c, peer)
	defer a.removePeer(c)

	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, dialTimeout)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		text := string(data)
		if strings.EqualFold(strings.TrimSpace(text), "ping") {
			wctx, wcancel := context.WithTimeout(ctx, dialTimeout)
			err := c.Write(wctx, websocket.MessageText, []byte("pong от "+host))
			wcancel()
			if err != nil {
				return
			}
			continue
		}
		m := a.inbox.add(peer, text)
		log.Printf("agent: сообщение #%d от %s (%d байт)", m.ID, peer, len(data))
		if a.onMessage != nil {
			a.onMessage(m)
		}
	}
}

// ---------------------------------------------------------------------------
// inbox — входящие сообщения для локального API
// ---------------------------------------------------------------------------

type message struct {
	ID   int       `json:"id"`
	Time time.Time `json:"time"`
	From string    `json:"from"`
	Text string    `json:"text"`
}

// inbox хранит последние capacity входящих сообщений с возрастающими ID и
// позволяет дождаться новых (wait). Пропадает при перезапуске программы.
type inbox struct {
	mu       sync.Mutex
	capacity int
	msgs     []message
	lastID   int
	changed  chan struct{} // закрывается и заменяется при каждом add
}

func newInbox(capacity int) *inbox {
	return &inbox{capacity: capacity, changed: make(chan struct{})}
}

func (b *inbox) add(from, text string) message {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastID++
	m := message{ID: b.lastID, Time: time.Now(), From: from, Text: text}
	b.msgs = append(b.msgs, m)
	if len(b.msgs) > b.capacity {
		b.msgs = slices.Delete(b.msgs, 0, len(b.msgs)-b.capacity)
	}
	close(b.changed)
	b.changed = make(chan struct{})
	return m
}

// after возвращает сообщения с ID больше id, последний выданный ID и канал,
// который закроется при следующем add.
func (b *inbox) after(id int) ([]message, int, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []message
	if i := slices.IndexFunc(b.msgs, func(m message) bool { return m.ID > id }); i >= 0 {
		out = slices.Clone(b.msgs[i:])
	}
	return out, b.lastID, b.changed
}

// wait ждёт, пока появятся сообщения с ID больше id, или до отмены ctx.
func (b *inbox) wait(ctx context.Context, id int) ([]message, int) {
	for {
		msgs, last, changed := b.after(id)
		if len(msgs) > 0 || ctx.Err() != nil {
			return msgs, last
		}
		select {
		case <-changed:
		case <-ctx.Done():
		}
	}
}

// ---------------------------------------------------------------------------
// Локальный API (только 127.0.0.1) — им пользуется скилл Claude
//
//	GET  /status                     состояние соединения, список пиров
//	POST /send?to=имя (тело — текст) отправить; без to — всем подключённым
//	GET  /inbox?after=N&wait=60      входящие с ID > N; wait — ждать новых
//
// Каждый запрос должен нести заголовок X-AI-Talk: 1, а Host — указывать на
// loopback. Это защита от страниц в браузере: кросс-доменный fetch не может
// поставить свой заголовок без CORS preflight (на который API не отвечает), а
// проверка Host отсекает DNS rebinding. Иначе любой сайт мог бы отправить
// второму Claude текст от имени пользователя.
// ---------------------------------------------------------------------------

func (a *agent) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		state, peer := a.State()
		_, last, _ := a.inbox.after(0)
		writeJSON(w, http.StatusOK, map[string]any{
			"host":    host,
			"mode":    a.cfg.mode,
			"enabled": a.Running(),
			"state":   state.String(),
			"peer":    peer,
			"peers":   a.PeerNames(),
			"last_id": last,
		})
	})
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessageSize))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": err.Error()})
			return
		}
		text := strings.TrimSpace(string(body))
		if text == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "пустое сообщение"})
			return
		}
		if !a.Running() {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "соединение выключено в трее ai-talk"})
			return
		}
		to := r.URL.Query().Get("to")
		sent, err := a.Send(r.Context(), to, text)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, errNotConnected) {
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]any{"error": err.Error()})
			return
		}
		log.Printf("api: отправлено %s (%d байт)", strings.Join(sent, ", "), len(text))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "to": sent})
	})
	mux.HandleFunc("GET /inbox", func(w http.ResponseWriter, r *http.Request) {
		after := 0
		if v := r.URL.Query().Get("after"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "after: " + err.Error()})
				return
			}
			after = n
		}
		var wait time.Duration
		if v := r.URL.Query().Get("wait"); v != "" {
			d, err := parseWait(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "wait: " + err.Error()})
				return
			}
			wait = d
		}
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		defer cancel()
		msgs, last := a.inbox.wait(ctx, after)
		if msgs == nil {
			msgs = []message{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "last_id": last})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(apiHeader) != "1" || !loopbackHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// parseWait разбирает wait как duration ("90s", "5m") или число секунд и
// ограничивает его maxWait.
func parseWait(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		n, nerr := strconv.Atoi(v)
		if nerr != nil {
			return 0, err
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 {
		return 0, errors.New("отрицательное время")
	}
	return min(d, maxWait), nil
}

func loopbackHost(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("api: запись ответа: %v", err)
	}
}

// serveAPI слушает локальный API на addr. Блокируется.
func serveAPI(addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("api: слушает http://%s", ln.Addr())
	return (&http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
}

// ---------------------------------------------------------------------------
// Баллон-уведомления
//
// Та же техника, что и в volume_switch: находит скрытое окно трея,
// созданное github.com/getlantern/systray (класс окна "SystrayClass", ID
// иконки 100 — оба значения захардкожены в systray v1.2.2), и вызывает
// Shell_NotifyIcon напрямую — публичного API для баллонов у systray нет.
// ---------------------------------------------------------------------------

const (
	nimModify = 0x00000001

	nifInfo = 0x00000010

	niifInfo             = 0x00000001
	niifNoSound          = 0x00000010
	niifRespectQuietTime = 0x00000080

	trayIconID = 100
)

// systrayClassName — класс скрытого окна трея; переменная только ради тестов.
var systrayClassName = "SystrayClass"

type notifyIconDataW struct {
	cbSize            uint32
	hWnd              windows.Handle
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             windows.Handle
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uTimeoutOrVersion uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          windows.GUID
	hBalloonIcon      windows.Handle
}

// showNotification displays a balloon notification with the given title and
// body. No-op if this process's tray window cannot be found.
func showNotification(title, body string, silent bool) {
	hwnd, err := findOwnTrayWindow()
	if err != nil {
		log.Printf("notification: поиск окна трея: %v", err)
		return
	}
	if hwnd == 0 {
		log.Printf("notification: окно трея %q не найдено, уведомление не показано", systrayClassName)
		return
	}

	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = trayIconID
	nid.uFlags = nifInfo
	nid.dwInfoFlags = niifInfo | niifRespectQuietTime
	if silent {
		nid.dwInfoFlags |= niifNoSound
	}
	copyUTF16(nid.szInfo[:], body)
	copyUTF16(nid.szInfoTitle[:], title)

	if r, _, errno := procShellNotifyIconW.Call(uintptr(nimModify), uintptr(unsafe.Pointer(&nid))); r == 0 {
		log.Printf("notification: %v", win32Error("Shell_NotifyIconW", errno))
	}
}

// copyUTF16 копирует s в нулевой буфер dst, при необходимости обрезая;
// последний элемент dst не трогается и остаётся завершающим нулём.
func copyUTF16(dst []uint16, s string) {
	src, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	copy(dst[:len(dst)-1], src)
}

// findOwnTrayWindow enumerates top-level windows looking for the hidden
// systray window that belongs to this process.
func findOwnTrayWindow() (windows.Handle, error) {
	pid := windows.GetCurrentProcessId()
	var found windows.Handle

	cb := windows.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		var winPid uint32
		if _, err := windows.GetWindowThreadProcessId(hwnd, &winPid); err != nil || winPid != pid {
			return 1
		}

		var cls [64]uint16
		n, err := windows.GetClassName(hwnd, &cls[0], int32(len(cls)))
		if err != nil {
			return 1
		}
		if windows.UTF16ToString(cls[:n]) == systrayClassName {
			found = windows.Handle(hwnd)
			return 0
		}
		return 1
	})

	if err := windows.EnumWindows(cb, nil); err != nil && found == 0 {
		return 0, fmt.Errorf("EnumWindows: %w", err)
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Генерация иконки трея — три цветных варианта (выключено/ожидание/
// подключено), без внешнего файла ресурсов, по образцу volume_switch.
// ---------------------------------------------------------------------------

var (
	iconOff       = renderTrayIcon(color.RGBA{R: 130, G: 130, B: 135, A: 255})
	iconWaiting   = renderTrayIcon(color.RGBA{R: 230, G: 180, B: 60, A: 255})
	iconConnected = renderTrayIcon(color.RGBA{R: 80, G: 200, B: 120, A: 255})
)

func renderTrayIcon(fg color.RGBA) []byte {
	const size = 32

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 30, G: 30, B: 35, A: 255}

	cx, cy, r := size/2, size/2, size/2-4
	for y := range size {
		for x := range size {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, fg)
			} else {
				img.Set(x, y, bg)
			}
		}
	}

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		panic(err)
	}

	return wrapICO(pngBuf.Bytes(), size, size)
}

// wrapICO wraps a single PNG image into a minimal valid .ico container.
func wrapICO(pngData []byte, w, h int) []byte {
	const headerSize = 6 + 16
	le := binary.LittleEndian
	b := make([]byte, 0, headerSize+len(pngData))

	b = le.AppendUint16(b, 0) // reserved
	b = le.AppendUint16(b, 1) // type: icon
	b = le.AppendUint16(b, 1) // image count

	b = append(b, byte(w), byte(h), 0, 0)        // width, height, color palette, reserved
	b = le.AppendUint16(b, 1)                    // color planes
	b = le.AppendUint16(b, 32)                   // bits per pixel
	b = le.AppendUint32(b, uint32(len(pngData))) // size of image data
	b = le.AppendUint32(b, headerSize)           // offset to image data

	return append(b, pngData...)
}

// ---------------------------------------------------------------------------
// Автозапуск через реестр (HKCU\...\Run)
// ---------------------------------------------------------------------------

const (
	autostartValueName = "AiTalk"
	autostartRunKey    = `Software\Microsoft\Windows\CurrentVersion\Run`

	settingsKey      = `Software\ai-talk` // настройки из меню трея (HKCU)
	showMessagesName = "ShowMessages"
)

// settings — флажки из меню трея, которые переживают перезапуск: DWORD-значения
// в HKCU\key (в программе — settingsKey, в тестах — временный ключ).
type settings struct {
	key string
}

// Bool возвращает значение name; def — если его ещё нет или его не прочитать.
func (s settings) Bool(name string, def bool) bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, s.key, registry.QUERY_VALUE)
	if err != nil {
		if !errors.Is(err, registry.ErrNotExist) {
			log.Printf("settings: открытие ключа: %v", err)
		}
		return def
	}
	defer logRegistryClose(key, s.key)
	v, _, err := key.GetIntegerValue(name)
	if err != nil {
		if !errors.Is(err, registry.ErrNotExist) {
			log.Printf("settings: чтение %s: %v", name, err)
		}
		return def
	}
	return v != 0
}

// SetBool сохраняет значение name, создавая ключ при необходимости.
func (s settings) SetBool(name string, v bool) (err error) {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, s.key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := key.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("закрытие ключа %s: %w", s.key, cerr))
		}
	}()
	var dw uint32
	if v {
		dw = 1
	}
	return key.SetDWordValue(name, dw)
}

// autostart — автозапуск программы через значение autostartValueName в
// ключе HKCU\key (в программе — autostartRunKey, в тестах — временный ключ).
type autostart struct {
	key string
}

// autostartCommand возвращает командную строку для записи в Run: путь к
// текущему exe в кавычках.
func autostartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `"`, nil
}

func logRegistryClose(key registry.Key, name string) {
	if err := key.Close(); err != nil {
		log.Printf("registry: закрытие ключа %s: %v", name, err)
	}
}

// Enabled проверяет, что значение в ключе автозапуска существует и указывает
// именно на текущий exe.
func (a autostart) Enabled() bool {
	cmd, err := autostartCommand()
	if err != nil {
		log.Printf("autostart: путь к exe: %v", err)
		return false
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, a.key, registry.QUERY_VALUE)
	if err != nil {
		log.Printf("autostart: открытие ключа Run: %v", err)
		return false
	}
	defer logRegistryClose(key, "Run")
	val, _, err := key.GetStringValue(autostartValueName)
	if err != nil {
		if !errors.Is(err, registry.ErrNotExist) {
			log.Printf("autostart: чтение %s: %v", autostartValueName, err)
		}
		return false
	}
	return val == cmd
}

// Set включает или выключает автозапуск.
func (a autostart) Set(enable bool) (err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, a.key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := key.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("закрытие ключа Run: %w", cerr))
		}
	}()

	if !enable {
		err := key.DeleteValue(autostartValueName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}

	cmd, err := autostartCommand()
	if err != nil {
		return err
	}
	return key.SetStringValue(autostartValueName, cmd)
}

// ---------------------------------------------------------------------------
// systray + соединение
// ---------------------------------------------------------------------------

// singleInstanceMutex — имя именованного мьютекса, по которому второй
// экземпляр узнаёт, что программа уже запущена. Переменная только ради
// тестов.
var singleInstanceMutex = `Local\ai_talk_single_instance`

// acquireSingleInstance создаёт именованный мьютекс. already=true, если
// мьютекс уже существует, т.е. другой экземпляр программы запущен.
func acquireSingleInstance() (release func(), already bool, err error) {
	name, err := windows.UTF16PtrFromString(singleInstanceMutex)
	if err != nil {
		return nil, false, fmt.Errorf("имя мьютекса: %w", err)
	}
	h, err := windows.CreateMutex(nil, false, name)
	if h == 0 {
		return nil, false, fmt.Errorf("CreateMutex: %w", err)
	}
	release = func() {
		if err := windows.CloseHandle(h); err != nil {
			log.Printf("single instance: закрытие мьютекса: %v", err)
		}
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		release()
		return nil, true, nil
	}
	return release, false, nil
}

func main() {
	listenFlag := flag.String("listen", "", "слушать подключения, например :8080 (перекрывает AGENT_LISTEN)")
	connectFlag := flag.String("connect", "", "адрес для подключения, например ws://HOST:8080/ws (перекрывает AGENT_CONNECT)")
	tokenFlag := flag.String("token", "", "общий секрет (перекрывает AGENT_TOKEN)")
	debug := flag.Bool("debug", false, "Показывать консоль с дебаг-логами")
	flag.Parse()

	closeLog := setupLogging(*debug)
	defer closeLog()

	releaseInstance, already, err := acquireSingleInstance()
	switch {
	case err != nil:
		log.Printf("single instance: %v — запускаюсь без проверки", err)
	case already:
		log.Printf("single instance: программа уже запущена, выхожу")
		return
	default:
		defer releaseInstance()
	}

	for _, path := range dotEnvPaths() {
		loadDotEnv(path)
	}
	cfg := resolveConfig(*debug, *listenFlag, *connectFlag, *tokenFlag)
	log.Printf("config: mode=%q listen=%q connect=%q api=%s настроено=%v debug=%v",
		cfg.mode, cfg.listenAddr, cfg.connectURL, cfg.apiAddr, cfg.configured(), cfg.debug)

	tray := &trayApp{debug: cfg.debug}
	tray.agent = newAgent(cfg, tray.onStateChange)
	tray.agent.onMessage = tray.onMessage
	tray.agent.onPeerConnected = tray.onPeerConnected
	tray.agent.onPeerDisconnected = tray.onPeerDisconnected
	go func() {
		if err := serveAPI(cfg.apiAddr, tray.agent.apiHandler()); err != nil {
			log.Printf("api: %v — скилл Claude работать не будет", err)
		}
	}()

	systray.Run(tray.onReady, tray.onExit)
}

// trayApp — программа с точки зрения трея: связывает меню и agent
// (включение/выключение соединения). onReady и onExit — колбэки
// systray.Run.
type trayApp struct {
	debug bool
	agent *agent

	// showMessages — показывать ли входящие сообщения уведомлением; читается
	// из горутины соединения, меняется из меню.
	showMessages atomic.Bool

	mState  *systray.MenuItem
	mToggle *systray.MenuItem
}

func (a *trayApp) onReady() {
	log.Printf("systray: onReady")
	if a.debug {
		if err := ensureConsole(); err != nil {
			log.Printf("console: %v", err)
		}
	}
	systray.SetIcon(iconOff)
	systray.SetTitle("")
	systray.SetTooltip("AI Talk: отключено")

	a.mState = systray.AddMenuItem(a.statusLabel(stateOff, ""), "")
	a.mState.Disable()
	systray.AddSeparator()

	a.mToggle = systray.AddMenuItem(toggleLabel(false), "Включить/выключить соединение между компьютерами")
	if !a.agent.cfg.configured() {
		a.mToggle.Disable()
		a.mToggle.SetTooltip("Не настроено: задайте AGENT_LISTEN или AGENT_CONNECT, и AGENT_TOKEN в .env")
	}
	go a.watchToggle()
	a.addShowMessagesItem(settings{key: settingsKey})

	systray.AddSeparator()
	a.addAutostartItem(autostart{key: autostartRunKey})
	a.addQuitItem()

	if a.agent.cfg.configured() {
		if err := a.agent.Start(); err != nil {
			log.Printf("agent: автозапуск соединения: %v", err)
		} else {
			a.mToggle.SetTitle(toggleLabel(true))
		}
	}
}

func (a *trayApp) onExit() {
	log.Printf("onExit: остановка")
	if a.agent != nil {
		a.agent.Stop()
	}
	log.Printf("onExit: завершено")
}

// watchToggle обрабатывает клики по пункту меню «Включить/выключить
// соединение».
func (a *trayApp) watchToggle() {
	for range a.mToggle.ClickedCh {
		if a.agent.Running() {
			log.Printf("tray: выключаю соединение")
			a.agent.Stop()
			a.mToggle.SetTitle(toggleLabel(false))
			continue
		}
		log.Printf("tray: включаю соединение")
		if err := a.agent.Start(); err != nil {
			log.Printf("agent: %v", err)
			showNotification("AI Talk", err.Error(), false)
			continue
		}
		a.mToggle.SetTitle(toggleLabel(true))
	}
}

// onStateChange вызывается agent'ом при каждом изменении состояния
// соединения и обновляет значок, подсказку и пункт статуса в меню; при
// установлении связи показывает уведомление.
func (a *trayApp) onStateChange(s connState, peer string) {
	label := a.statusLabel(s, peer)
	log.Printf("agent: %s", label)
	if a.mState != nil {
		a.mState.SetTitle(label)
	}
	switch s {
	case stateOff:
		systray.SetIcon(iconOff)
		systray.SetTooltip("AI Talk: отключено")
		if a.mToggle != nil {
			a.mToggle.SetTitle(toggleLabel(false))
		}
	case stateWaiting:
		systray.SetIcon(iconWaiting)
		systray.SetTooltip("AI Talk: " + label)
	case stateConnected:
		systray.SetIcon(iconConnected)
		systray.SetTooltip("AI Talk: подключено к " + peer)
	}
}

// onPeerConnected показывает уведомление о подключении конкретного
// собеседника — отдельно от onStateChange, чтобы не пропустить второго и
// последующих пиров, когда кто-то уже был подключён.
func (a *trayApp) onPeerConnected(peer string) {
	showNotification("AI Talk", "Подключено: "+peer, false)
}

func (a *trayApp) onPeerDisconnected(peer string) {
	log.Printf("tray: отключился %s", peer)
}

// addShowMessagesItem добавляет пункт с галочкой, включающий/выключающий
// уведомления о входящих сообщениях. Сами сообщения по-прежнему попадают во
// входящие API — выключается только показ.
func (a *trayApp) addShowMessagesItem(s settings) {
	a.showMessages.Store(s.Bool(showMessagesName, true))
	item := systray.AddMenuItemCheckbox(
		"Показывать входящие сообщения",
		"Всплывающее уведомление на каждое сообщение со второго компьютера",
		a.showMessages.Load(),
	)
	go func() {
		for range item.ClickedCh {
			show := !item.Checked()
			if err := s.SetBool(showMessagesName, show); err != nil {
				log.Printf("settings: не удалось сохранить %s: %v", showMessagesName, err)
			}
			a.showMessages.Store(show)
			if show {
				item.Check()
			} else {
				item.Uncheck()
			}
			log.Printf("tray: показ сообщений=%v", show)
		}
	}()
}

// onMessage показывает входящее сообщение всплывающим уведомлением, если
// показ включён в трее.
func (a *trayApp) onMessage(m message) {
	if !a.showMessages.Load() {
		return
	}
	text := m.Text
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200]) + "…"
	}
	showNotification("AI Talk: сообщение от "+m.From, text, false)
}

// statusLabel формирует текст пункта статуса в меню трея.
func (a *trayApp) statusLabel(s connState, peer string) string {
	switch s {
	case stateConnected:
		return "Статус: подключено к " + peer
	case stateWaiting:
		if a.agent.cfg.mode == "server" {
			return "Статус: ожидание подключения на " + a.agent.cfg.listenAddr
		}
		return "Статус: подключение к " + a.agent.cfg.connectURL
	default:
		return "Статус: отключено"
	}
}

func toggleLabel(on bool) string {
	if on {
		return "Выключить соединение"
	}
	return "Включить соединение"
}

// addAutostartItem добавляет пункт меню с галочкой, включающий/выключающий
// автозапуск.
func (a *trayApp) addAutostartItem(run autostart) {
	item := systray.AddMenuItemCheckbox(
		"Запускать при входе в Windows",
		"Добавить/убрать из автозагрузки Windows",
		run.Enabled(),
	)
	go func() {
		for range item.ClickedCh {
			enable := !item.Checked()
			if err := run.Set(enable); err != nil {
				log.Printf("autostart: не удалось изменить: %v", err)
				continue
			}
			if enable {
				item.Check()
			} else {
				item.Uncheck()
			}
			log.Printf("autostart: включён=%v", enable)
		}
	}()
}

func (a *trayApp) addQuitItem() {
	item := systray.AddMenuItem("Выход", "Закрыть AI Talk")
	go func() {
		<-item.ClickedCh
		log.Printf("tray: «Выход» нажат")
		systray.Quit()
	}()
}
