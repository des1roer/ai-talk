package com.des1roer.aitalk

import android.content.Context
import android.os.Build

class SettingsStore(context: Context) {
    private val prefs = context.applicationContext.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    var host: String
        get() = prefs.getString(KEY_HOST, "") ?: ""
        set(value) = prefs.edit().putString(KEY_HOST, value).apply()

    var port: String
        get() = prefs.getString(KEY_PORT, "8080") ?: "8080"
        set(value) = prefs.edit().putString(KEY_PORT, value).apply()

    var token: String
        get() = prefs.getString(KEY_TOKEN, "") ?: ""
        set(value) = prefs.edit().putString(KEY_TOKEN, value).apply()

    var deviceName: String
        get() = prefs.getString(KEY_DEVICE_NAME, Build.MODEL) ?: Build.MODEL
        set(value) = prefs.edit().putString(KEY_DEVICE_NAME, value).apply()

    val isConfigured: Boolean
        get() = host.isNotBlank() && token.isNotBlank()

    companion object {
        private const val PREFS_NAME = "ai_talk_settings"
        private const val KEY_HOST = "host"
        private const val KEY_PORT = "port"
        private const val KEY_TOKEN = "token"
        private const val KEY_DEVICE_NAME = "device_name"
    }
}
