package com.des1roer.aitalk

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.viewModels
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue

class MainActivity : ComponentActivity() {
    private val viewModel: ConnectionViewModel by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            AiTalkTheme {
                AiTalkApp(viewModel)
            }
        }
    }
}

private enum class Screen { Chat, Settings }

@Composable
private fun AiTalkApp(viewModel: ConnectionViewModel) {
    var screen by rememberSaveable { mutableStateOf(Screen.Chat) }
    when (screen) {
        Screen.Chat -> ChatScreen(viewModel = viewModel, onOpenSettings = { screen = Screen.Settings })
        Screen.Settings -> SettingsScreen(viewModel = viewModel, onBack = { screen = Screen.Chat })
    }
}
