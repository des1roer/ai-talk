package com.des1roer.aitalk

data class ChatMessage(
    val id: Long,
    val fromSelf: Boolean,
    val text: String,
    val timestamp: Long,
    val failed: Boolean = false,
)
