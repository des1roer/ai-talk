package com.des1roer.aitalk

import java.util.concurrent.TimeUnit
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener

private const val RECONNECT_DELAY_MS = 3_000L

class ConnectionManager(private val settings: SettingsStore, private val messageStore: MessageStore) {
    private val client = OkHttpClient.Builder()
        .pingInterval(20, TimeUnit.SECONDS)
        .build()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private var job: Job? = null
    private var webSocket: WebSocket? = null

    private val storedMessages = messageStore.load()
    private var nextId = (storedMessages.maxOfOrNull { it.id } ?: -1L) + 1

    @Volatile
    private var running = false

    private val _state = MutableStateFlow<ConnectionState>(ConnectionState.Off)
    val state: StateFlow<ConnectionState> = _state.asStateFlow()

    private val _messages = MutableStateFlow(storedMessages)
    val messages: StateFlow<List<ChatMessage>> = _messages.asStateFlow()

    fun isRunning(): Boolean = running

    fun start() {
        if (running) return
        running = true
        _state.value = ConnectionState.Waiting
        job = scope.launch { reconnectLoop() }
    }

    fun stop() {
        running = false
        job?.cancel()
        job = null
        webSocket?.close(1000, "user stop")
        webSocket = null
        _state.value = ConnectionState.Off
    }

    fun send(text: String): Boolean {
        val ok = trySend(text)
        appendMessage(fromSelf = true, text = text, failed = !ok)
        return ok
    }

    fun retry(id: Long) {
        val message = _messages.value.find { it.id == id } ?: return
        val ok = trySend(message.text)
        _messages.value = _messages.value.map { if (it.id == id) it.copy(failed = !ok) else it }
        persist()
    }

    private fun trySend(text: String): Boolean {
        val socket = webSocket ?: return false
        if (_state.value !is ConnectionState.Connected) return false
        return socket.send(text)
    }

    private suspend fun reconnectLoop() {
        while (running) {
            val closed = CompletableDeferred<Unit>()
            val request = Request.Builder()
                .url("ws://${settings.host}:${settings.port}/ws")
                .addHeader("Authorization", "Bearer ${settings.token}")
                .addHeader("X-Agent-Host", settings.deviceName)
                .build()

            val listener = object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) {
                    val peer = response.header("X-Agent-Host")?.takeIf { it.isNotBlank() } ?: settings.host
                    _state.value = ConnectionState.Connected(peer)
                }

                override fun onMessage(webSocket: WebSocket, text: String) {
                    if (text.trim().equals("ping", ignoreCase = true)) {
                        webSocket.send("pong от ${settings.deviceName}")
                    } else {
                        appendMessage(fromSelf = false, text = text)
                    }
                }

                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                    if (running) _state.value = ConnectionState.Waiting
                    closed.complete(Unit)
                }

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                    if (running) _state.value = ConnectionState.Waiting
                    closed.complete(Unit)
                }
            }

            webSocket = client.newWebSocket(request, listener)
            closed.await()
            webSocket = null
            if (running) delay(RECONNECT_DELAY_MS)
        }
        _state.value = ConnectionState.Off
    }

    private fun appendMessage(fromSelf: Boolean, text: String, failed: Boolean = false) {
        val message = ChatMessage(
            id = nextId++,
            fromSelf = fromSelf,
            text = text,
            timestamp = System.currentTimeMillis(),
            failed = failed,
        )
        _messages.value = _messages.value + message
        persist()
    }

    private fun persist() {
        val snapshot = _messages.value
        scope.launch { messageStore.save(snapshot) }
    }
}
