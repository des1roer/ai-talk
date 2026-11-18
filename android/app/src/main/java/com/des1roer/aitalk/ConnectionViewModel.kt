package com.des1roer.aitalk

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import kotlinx.coroutines.flow.StateFlow

class ConnectionViewModel(application: Application) : AndroidViewModel(application) {
    val settingsStore = SettingsStore(application)
    private val manager = ConnectionManager(settingsStore)

    val state: StateFlow<ConnectionState> = manager.state
    val messages: StateFlow<List<ChatMessage>> = manager.messages

    init {
        if (settingsStore.isConfigured) manager.start()
    }

    fun toggleConnection() {
        if (manager.isRunning()) manager.stop() else manager.start()
    }

    fun send(text: String): Boolean = manager.send(text)

    fun restartWithNewSettings() {
        val wasRunning = manager.isRunning()
        manager.stop()
        if (wasRunning) manager.start()
    }
}
