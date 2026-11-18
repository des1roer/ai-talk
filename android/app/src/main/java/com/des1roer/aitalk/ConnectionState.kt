package com.des1roer.aitalk

sealed interface ConnectionState {
    data object Off : ConnectionState
    data object Waiting : ConnectionState
    data class Connected(val peer: String) : ConnectionState
}
