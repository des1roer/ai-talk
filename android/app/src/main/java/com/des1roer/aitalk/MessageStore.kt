package com.des1roer.aitalk

import android.content.Context
import java.io.File
import org.json.JSONArray
import org.json.JSONObject

private const val MAX_STORED_MESSAGES = 500

class MessageStore(context: Context) {
    private val file = File(context.applicationContext.filesDir, "messages.json")

    fun load(): List<ChatMessage> {
        if (!file.exists()) return emptyList()
        return runCatching {
            val array = JSONArray(file.readText())
            (0 until array.length()).map { i ->
                val o = array.getJSONObject(i)
                ChatMessage(
                    id = o.getLong("id"),
                    fromSelf = o.getBoolean("fromSelf"),
                    text = o.getString("text"),
                    timestamp = o.getLong("timestamp"),
                    failed = o.optBoolean("failed", false),
                )
            }
        }.getOrDefault(emptyList())
    }

    fun save(messages: List<ChatMessage>) {
        val array = JSONArray()
        messages.takeLast(MAX_STORED_MESSAGES).forEach { m ->
            array.put(
                JSONObject()
                    .put("id", m.id)
                    .put("fromSelf", m.fromSelf)
                    .put("text", m.text)
                    .put("timestamp", m.timestamp)
                    .put("failed", m.failed),
            )
        }
        file.writeText(array.toString())
    }
}
