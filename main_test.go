package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/windows/registry"
)

// ---------------------------------------------------------------------------
// Конфигурация
// ---------------------------------------------------------------------------

func TestResolveConfig(t *testing.T) {
	for _, k := range []string{"AGENT_LISTEN", "AGENT_CONNECT", "AGENT_TOKEN"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	// Без файла токена рядом с тестовым бинарём resolveToken() не находит ничего лишнего.
	setVar(t, &dataDir, func() (string, error) { return t.TempDir(), nil })

	tests := []struct {
		name                               string
		env                                map[string]string
		listenFlag, connectFlag, tokenFlag string
		wantMode                           string
		wantListen, wantConnect, wantToken string
	}{
		{
			name:     "ничего не задано",
			wantMode: "",
		},
		{
			name:       "AGENT_LISTEN из окружения",
			env:        map[string]string{"AGENT_LISTEN": ":8080", "AGENT_TOKEN": "secret"},
			wantMode:   "server",
			wantListen: ":8080",
			wantToken:  "secret",
		},
		{
			name:        "AGENT_CONNECT из окружения",
			env:         map[string]string{"AGENT_CONNECT": "ws://host:8080/ws", "AGENT_TOKEN": "secret"},
			wantMode:    "client",
			wantConnect: "ws://host:8080/ws",
			wantToken:   "secret",
		},
		{
			name:       "флаг перекрывает окружение",
			env:        map[string]string{"AGENT_LISTEN": ":8080"},
			listenFlag: ":9090",
			wantMode:   "server",
			wantListen: ":9090",
		},
		{
			name:        "заданы оба — побеждает сервер",
			env:         map[string]string{"AGENT_LISTEN": ":8080", "AGENT_CONNECT": "ws://host:8080/ws"},
			wantMode:    "server",
			wantListen:  ":8080",
			wantConnect: "ws://host:8080/ws",
		},
		{
			name:      "токен флагом",
			tokenFlag: "from-flag",
			wantToken: "from-flag",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"AGENT_LISTEN", "AGENT_CONNECT", "AGENT_TOKEN"} {
				t.Setenv(k, tt.env[k])
				if tt.env[k] == "" {
					if err := os.Unsetenv(k); err != nil {
						t.Fatal(err)
					}
				}
			}
			cfg := resolveConfig(false, tt.listenFlag, tt.connectFlag, tt.tokenFlag)
			if cfg.mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", cfg.mode, tt.wantMode)
			}
			if cfg.listenAddr != tt.wantListen {
				t.Errorf("listenAddr = %q, want %q", cfg.listenAddr, tt.wantListen)
			}
			if cfg.connectURL != tt.wantConnect {
				t.Errorf("connectURL = %q, want %q", cfg.connectURL, tt.wantConnect)
			}
			if cfg.token != tt.wantToken {
				t.Errorf("token = %q, want %q", cfg.token, tt.wantToken)
			}
		})
	}
}

func TestResolveTokenFromFile(t *testing.T) {
	dir := t.TempDir()
	setVar(t, &dataDir, func() (string, error) { return dir, nil })
	for _, k := range []string{"AGENT_TOKEN"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}

	if got := resolveToken(""); got != "" {
		t.Errorf("без файла: resolveToken() = %q, want \"\"", got)
	}

	if err := os.WriteFile(filepath.Join(dir, tokenFileName), []byte("  from-file  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolveToken(""); got != "from-file" {
		t.Errorf("resolveToken() = %q, want %q", got, "from-file")
	}
	if got := resolveToken("from-flag"); got != "from-flag" {
		t.Errorf("флаг должен перекрывать файл: resolveToken() = %q", got)
	}
}

func TestConfigConfigured(t *testing.T) {
	tests := []struct {
		cfg  config
		want bool
	}{
		{config{}, false},
		{config{mode: "server"}, false},
		{config{mode: "server", token: "t"}, true},
		{config{token: "t"}, false},
	}
	for _, tt := range tests {
		if got := tt.cfg.configured(); got != tt.want {
			t.Errorf("config{mode:%q,token:%q}.configured() = %v, want %v", tt.cfg.mode, tt.cfg.token, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// .env
// ---------------------------------------------------------------------------

func TestDotEnvPaths(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exeDir := filepath.Dir(exe)
	wd, data := t.TempDir(), t.TempDir()
	t.Chdir(wd)
	setVar(t, &dataDir, func() (string, error) { return data, nil })
	want := []string{
		filepath.Join(wd, ".env"),
		filepath.Join(exeDir, ".env"),
		filepath.Join(data, ".env"),
	}
	if got := dotEnvPaths(); !slices.Equal(got, want) {
		t.Errorf("dotEnvPaths() = %q, want %q", got, want)
	}

	t.Chdir(exeDir)
	want = []string{filepath.Join(exeDir, ".env")}
	setVar(t, &dataDir, func() (string, error) { return strings.ToUpper(exeDir), nil })
	if got := dotEnvPaths(); !slices.Equal(got, want) {
		t.Errorf("с каталогом данных = каталогу exe: dotEnvPaths() = %q, want %q", got, want)
	}
}

func TestLoadDotEnv(t *testing.T) {
	const (
		keyPlain    = "AT_TEST_PLAIN"
		keyQuoted   = "AT_TEST_QUOTED"
		keyExisting = "AT_TEST_EXISTING"
		keyComment  = "AT_TEST_COMMENTED"
	)
	for _, k := range []string{keyPlain, keyQuoted, keyComment} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s уже задан в окружении", k)
		}
		t.Cleanup(func() { _ = os.Unsetenv(k) })
	}
	t.Setenv(keyExisting, "outer")

	content := strings.Join([]string{
		"# комментарий",
		"#" + keyComment + "=nope",
		"",
		"  " + keyPlain + " = value  ",
		keyQuoted + `="C:\Program Files\x.exe"`,
		keyExisting + "=inner",
		"=пустой ключ",
	}, "\n")

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	loadDotEnv(path)

	if got := os.Getenv(keyPlain); got != "value" {
		t.Errorf("%s = %q, want %q", keyPlain, got, "value")
	}
	if got := os.Getenv(keyQuoted); got != `C:\Program Files\x.exe` {
		t.Errorf("%s = %q", keyQuoted, got)
	}
	if got := os.Getenv(keyExisting); got != "outer" {
		t.Errorf("уже заданная извне переменная перезаписана: %s = %q", keyExisting, got)
	}
	if _, ok := os.LookupEnv(keyComment); ok {
		t.Errorf("закомментированная строка загружена")
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	loadDotEnv(filepath.Join(t.TempDir(), "nope.env"))
}

// ---------------------------------------------------------------------------
// Значок трея
// ---------------------------------------------------------------------------

func TestWrapICO(t *testing.T) {
	data := wrapICO([]byte{1, 2, 3, 4}, 32, 32)
	if len(data) != 6+16+4 {
		t.Fatalf("len = %d", len(data))
	}
	if data[2] != 1 || data[3] != 0 {
		t.Errorf("ICONDIR.count не равен 1: %v", data[2:4])
	}
	if data[6] != 32 || data[7] != 32 {
		t.Errorf("ширина/высота в ICONDIRENTRY = %d/%d", data[6], data[7])
	}
}

func TestRenderTrayIcon(t *testing.T) {
	for _, icon := range [][]byte{iconOff, iconWaiting, iconConnected} {
		if len(icon) == 0 {
			t.Fatal("renderTrayIcon() вернул пустой значок")
		}
	}
}

// ---------------------------------------------------------------------------
// Метки статуса/переключателя
// ---------------------------------------------------------------------------

func TestToggleLabel(t *testing.T) {
	if got := toggleLabel(false); got != "Включить соединение" {
		t.Errorf("toggleLabel(false) = %q", got)
	}
	if got := toggleLabel(true); got != "Выключить соединение" {
		t.Errorf("toggleLabel(true) = %q", got)
	}
}

func TestStatusLabel(t *testing.T) {
	serverApp := &trayApp{agent: &agent{cfg: config{mode: "server", listenAddr: ":8080"}}}
	if got := serverApp.statusLabel(stateWaiting, ""); got != "Статус: ожидание подключения на :8080" {
		t.Errorf("server waiting: %q", got)
	}
	if got := serverApp.statusLabel(stateConnected, "DESKTOP"); got != "Статус: подключено к DESKTOP" {
		t.Errorf("connected: %q", got)
	}
	if got := serverApp.statusLabel(stateOff, ""); got != "Статус: отключено" {
		t.Errorf("off: %q", got)
	}

	clientApp := &trayApp{agent: &agent{cfg: config{mode: "client", connectURL: "ws://host/ws"}}}
	if got := clientApp.statusLabel(stateWaiting, ""); got != "Статус: подключение к ws://host/ws" {
		t.Errorf("client waiting: %q", got)
	}
}

// ---------------------------------------------------------------------------
// agent: включение/выключение без настоящей сети
// ---------------------------------------------------------------------------

func TestAgentStartNotConfigured(t *testing.T) {
	a := newAgent(config{}, nil)
	if err := a.Start(); err == nil {
		t.Error("Start() без настроек не вернул ошибку")
	}
	if a.Running() {
		t.Error("Running() = true после неудачного Start()")
	}
}

func TestAgentServerStartStop(t *testing.T) {
	var states []connState
	var mu sync.Mutex
	a := newAgent(config{mode: "server", listenAddr: "127.0.0.1:0", token: "t"}, func(s connState, _ string) {
		mu.Lock()
		states = append(states, s)
		mu.Unlock()
	})

	if err := a.Start(); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if !a.Running() {
		t.Error("Running() = false сразу после Start()")
	}
	// Повторный Start — не ошибка и не второй запуск.
	if err := a.Start(); err != nil {
		t.Fatalf("повторный Start(): %v", err)
	}

	a.Stop()
	if a.Running() {
		t.Error("Running() = true после Stop()")
	}
	if s, _ := a.State(); s != stateOff {
		t.Errorf("State() после Stop() = %v, want stateOff", s)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(states) == 0 || states[0] != stateWaiting {
		t.Errorf("states = %v, первое должно быть stateWaiting", states)
	}
}

// waitForState ждёт, пока agent перейдёт в состояние want (переходы
// асинхронны — их делают фоновые горутины runServer/runClient).
func waitForState(t *testing.T, a *agent, want connState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := a.State(); s == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s, _ := a.State()
	t.Fatalf("State() = %v после ожидания, want %v", s, want)
}

// freeAddr находит свободный TCP-порт на 127.0.0.1: слушает и сразу
// закрывает, оставляя агенту переиспользовать тот же адрес.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestAgentServerAcceptsConnection(t *testing.T) {
	addr := freeAddr(t)
	a := newAgent(config{mode: "server", listenAddr: addr, token: "secret"}, nil)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	waitForState(t, a, stateWaiting)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer secret"}, "X-Agent-Host": {"PEER-PC"}},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	waitForState(t, a, stateConnected)
	if s, peer := a.State(); peer != "PEER-PC" {
		t.Errorf("State() = (%v, %q), want peer PEER-PC", s, peer)
	}

	c.CloseNow()
	waitForState(t, a, stateWaiting)
}

func TestAgentServerRejectsBadToken(t *testing.T) {
	addr := freeAddr(t)
	a := newAgent(config{mode: "server", listenAddr: addr, token: "secret"}, nil)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	waitForState(t, a, stateWaiting)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}},
	})
	if err == nil {
		t.Fatal("Dial с неверным токеном не вернул ошибку")
	}

	// Состояние не должно смениться на «подключено».
	time.Sleep(100 * time.Millisecond)
	if s, _ := a.State(); s != stateWaiting {
		t.Errorf("State() = %v после отказа, want stateWaiting", s)
	}
}

func TestAgentServerRejectsSecondConnection(t *testing.T) {
	addr := freeAddr(t)
	a := newAgent(config{mode: "server", listenAddr: addr, token: "secret"}, nil)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	waitForState(t, a, stateWaiting)

	dial := func(peer string) *websocket.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer secret"}, "X-Agent-Host": {peer}},
		})
		if err != nil {
			t.Fatalf("Dial(%s): %v", peer, err)
		}
		return c
	}

	first := dial("FIRST")
	defer first.CloseNow()
	waitForState(t, a, stateConnected)

	second := dial("SECOND")
	defer second.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := second.Read(ctx); err == nil {
		t.Error("второе соединение не было закрыто сервером")
	}
	if _, peer := a.State(); peer != "FIRST" {
		t.Errorf("peer = %q, want FIRST (второе соединение не должно было вытеснить первое)", peer)
	}
}

func TestAgentClientConnects(t *testing.T) {
	var gotAuth, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.Header.Get("X-Agent-Host")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.Read(r.Context())
	}))
	defer srv.Close()

	a := newAgent(config{
		mode:       "client",
		connectURL: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		token:      "secret",
	}, nil)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	waitForState(t, a, stateConnected)
	if gotAuth != "Bearer secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotHost != host {
		t.Errorf("X-Agent-Host = %q, want %q", gotHost, host)
	}
}

// ---------------------------------------------------------------------------
// Обмен сообщениями и локальный API
// ---------------------------------------------------------------------------

// connectedPair поднимает в процессе сервер и клиент ai-talk и ждёт, пока они
// соединятся.
func connectedPair(t *testing.T) (server, client *agent) {
	t.Helper()
	addr := freeAddr(t)
	server = newAgent(config{mode: "server", listenAddr: addr, token: "secret"}, nil)
	client = newAgent(config{mode: "client", connectURL: "ws://" + addr + "/ws", token: "secret"}, nil)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	waitForState(t, server, stateWaiting)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Stop)
	waitForState(t, client, stateConnected)
	waitForState(t, server, stateConnected)
	return server, client
}

func waitInbox(t *testing.T, a *agent, after int) []message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, _ := a.inbox.wait(ctx, after)
	if len(msgs) == 0 {
		t.Fatal("сообщение не пришло")
	}
	return msgs
}

func TestAgentsExchangeMessages(t *testing.T) {
	server, client := connectedPair(t)

	if _, peer := client.State(); peer != host {
		t.Errorf("клиент: peer = %q, want имя сервера %q из X-Agent-Host", peer, host)
	}

	long := strings.Repeat("я", 100_000) // больше 32 КБ — лимита websocket по умолчанию
	if err := client.Send(context.Background(), long); err != nil {
		t.Fatalf("client.Send: %v", err)
	}
	got := waitInbox(t, server, 0)
	if got[0].Text != long || got[0].From != host || got[0].ID != 1 {
		t.Errorf("сервер получил #%d от %q, %d байт", got[0].ID, got[0].From, len(got[0].Text))
	}

	if err := server.Send(context.Background(), "ответ"); err != nil {
		t.Fatalf("server.Send: %v", err)
	}
	if got := waitInbox(t, client, 0); got[0].Text != "ответ" {
		t.Errorf("клиент получил %q", got[0].Text)
	}
}

func TestAgentAnswersPing(t *testing.T) {
	server, client := connectedPair(t)
	if err := client.Send(context.Background(), "ping"); err != nil {
		t.Fatal(err)
	}
	got := waitInbox(t, client, 0)
	if want := "pong от " + host; got[0].Text != want {
		t.Errorf("ответ на ping = %q, want %q", got[0].Text, want)
	}
	if msgs, _, _ := server.inbox.after(0); len(msgs) != 0 {
		t.Errorf("ping попал во входящие сервера: %v", msgs)
	}
}

func TestAgentSendNotConnected(t *testing.T) {
	a := newAgent(config{}, nil)
	if err := a.Send(context.Background(), "x"); !errors.Is(err, errNotConnected) {
		t.Errorf("Send без соединения = %v, want errNotConnected", err)
	}
}

func TestInbox(t *testing.T) {
	b := newInbox(3)
	for i := range 5 {
		b.add("P", fmt.Sprint(i))
	}
	msgs, last, _ := b.after(0)
	if last != 5 || len(msgs) != 3 || msgs[0].ID != 3 {
		t.Fatalf("after(0) = %v, last %d; want последние 3 из 5", msgs, last)
	}
	if msgs, _, _ := b.after(4); len(msgs) != 1 || msgs[0].Text != "4" {
		t.Errorf("after(4) = %v", msgs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if msgs, _ := b.wait(ctx, 5); len(msgs) != 0 {
		t.Errorf("wait без новых сообщений вернул %v", msgs)
	}

	done := make(chan []message)
	go func() {
		msgs, _ := b.wait(context.Background(), 5)
		done <- msgs
	}()
	time.Sleep(10 * time.Millisecond)
	b.add("P", "новое")
	select {
	case msgs := <-done:
		if len(msgs) != 1 || msgs[0].Text != "новое" {
			t.Errorf("wait вернул %v", msgs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait не проснулся после add")
	}
}

func TestParseWait(t *testing.T) {
	for in, want := range map[string]time.Duration{"90": 90 * time.Second, "2m": 2 * time.Minute, "5h": maxWait} {
		if got, err := parseWait(in); err != nil || got != want {
			t.Errorf("parseWait(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "-1s"} {
		if _, err := parseWait(in); err == nil {
			t.Errorf("parseWait(%q): ожидалась ошибка", in)
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	for in, want := range map[string]bool{
		"127.0.0.1:8767": true, "localhost:8767": true, "[::1]:8767": true, "127.0.0.1": true,
		"evil.example:8767": false, "192.168.3.2:8767": false,
	} {
		if got := loopbackHost(in); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", in, got, want)
		}
	}
}

// apiCall выполняет запрос к локальному API и разбирает JSON-ответ.
func apiCall(t *testing.T, method, url, body string, header bool) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if header {
		req.Header.Set(apiHeader, "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAPI(t *testing.T) {
	server, client := connectedPair(t)
	api := httptest.NewServer(client.apiHandler())
	defer api.Close()

	if code, _ := apiCall(t, "GET", api.URL+"/status", "", false); code != http.StatusForbidden {
		t.Errorf("без заголовка %s: код %d, want 403", apiHeader, code)
	}

	code, st := apiCall(t, "GET", api.URL+"/status", "", true)
	if code != http.StatusOK || st["state"] != "connected" || st["enabled"] != true || st["peer"] != host {
		t.Errorf("/status = %d %v", code, st)
	}

	if code, _ := apiCall(t, "POST", api.URL+"/send", "  ", true); code != http.StatusBadRequest {
		t.Errorf("пустое сообщение: код %d", code)
	}
	const text = "привет\nвторая строка"
	if code, res := apiCall(t, "POST", api.URL+"/send", text, true); code != http.StatusOK {
		t.Errorf("/send = %d %v", code, res)
	}
	if got := waitInbox(t, server, 0); got[0].Text != text {
		t.Errorf("сервер получил %q", got[0].Text)
	}

	if err := server.Send(context.Background(), "ответ сервера"); err != nil {
		t.Fatal(err)
	}
	code, res := apiCall(t, "GET", api.URL+"/inbox?after=0&wait=5", "", true)
	msgs, _ := res["messages"].([]any)
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].(map[string]any)["text"] != "ответ сервера" || res["last_id"] != float64(1) {
		t.Errorf("/inbox = %d %v", code, res)
	}
	start := time.Now()
	code, res = apiCall(t, "GET", api.URL+"/inbox?after=1&wait=0.2s", "", true)
	if msgs, _ := res["messages"].([]any); code != http.StatusOK || len(msgs) != 0 || time.Since(start) < 150*time.Millisecond {
		t.Errorf("/inbox без новых = %d %v за %s", code, res, time.Since(start))
	}
	if code, _ := apiCall(t, "GET", api.URL+"/inbox?after=x", "", true); code != http.StatusBadRequest {
		t.Errorf("after=x: код %d", code)
	}

	client.Stop()
	if code, res := apiCall(t, "POST", api.URL+"/send", "x", true); code != http.StatusConflict {
		t.Errorf("/send при выключенном соединении = %d %v, want 409", code, res)
	}
}

func TestAPIRejectsForeignHost(t *testing.T) {
	api := httptest.NewServer(newAgent(config{}, nil).apiHandler())
	defer api.Close()
	req, _ := http.NewRequest("GET", api.URL+"/status", nil)
	req.Host = "evil.example"
	req.Header.Set(apiHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("чужой Host: код %d, want 403", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Автозапуск
// ---------------------------------------------------------------------------

func TestAutostart(t *testing.T) {
	run := autostart{key: tempRegistryKey(t)}

	if run.Enabled() {
		t.Error("пустой ключ: автозапуск включён")
	}
	if err := run.Set(true); err != nil {
		t.Fatalf("Set(true): %v", err)
	}
	if !run.Enabled() {
		t.Error("после включения автозапуск выключен")
	}

	setRegistryString(t, run.key, autostartValueName, `"C:\old\ai-talk.exe"`)
	if run.Enabled() {
		t.Error("значение указывает на другой exe, а автозапуск считается включённым")
	}

	if err := run.Set(false); err != nil {
		t.Fatalf("Set(false): %v", err)
	}
	if run.Enabled() {
		t.Error("после выключения автозапуск включён")
	}
	if err := run.Set(false); err != nil {
		t.Errorf("повторное выключение: %v", err)
	}
}

func TestSettings(t *testing.T) {
	s := settings{key: tempRegistryKey(t)}
	if !s.Bool(showMessagesName, true) || s.Bool(showMessagesName, false) {
		t.Error("нет значения: должно вернуться умолчание")
	}
	if err := s.SetBool(showMessagesName, false); err != nil {
		t.Fatal(err)
	}
	if s.Bool(showMessagesName, true) {
		t.Error("после SetBool(false) значение true")
	}
	if err := s.SetBool(showMessagesName, true); err != nil {
		t.Fatal(err)
	}
	if !s.Bool(showMessagesName, false) {
		t.Error("после SetBool(true) значение false")
	}

	missing := settings{key: `Software\ai_talk_test_no_such_key`}
	if !missing.Bool(showMessagesName, true) {
		t.Error("нет ключа: должно вернуться умолчание")
	}
}

func TestAutostartMissingKey(t *testing.T) {
	run := autostart{key: `Software\ai_talk_test_no_such_key`}
	if run.Enabled() {
		t.Error("нет ключа: автозапуск включён")
	}
	if err := run.Set(true); err == nil {
		t.Error("нет ключа: ожидалась ошибка")
	}
}

func TestAutostartCommand(t *testing.T) {
	cmd, err := autostartCommand()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := `"` + exe + `"`; cmd != want {
		t.Errorf("got %q, want %q", cmd, want)
	}
}

// ---------------------------------------------------------------------------
// Хелперы для тестов, работающих с системой.
// ---------------------------------------------------------------------------

func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func tempRegistryKey(t *testing.T) string {
	t.Helper()
	path := fmt.Sprintf(`Software\ai_talk_test_%d_%s`, os.Getpid(), t.Name())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("создание %s: %v", path, err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.DeleteKey(registry.CURRENT_USER, path); err != nil {
			t.Errorf("удаление %s: %v", path, err)
		}
	})
	return path
}

func setRegistryString(t *testing.T, path, name, value string) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetStringValue(name, value); err != nil {
		t.Fatal(err)
	}
}
