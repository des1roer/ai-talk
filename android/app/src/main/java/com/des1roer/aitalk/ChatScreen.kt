package com.des1roer.aitalk

import android.text.format.DateFormat
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ChatScreen(viewModel: ConnectionViewModel, onOpenSettings: () -> Unit) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val messages by viewModel.messages.collectAsStateWithLifecycle()
    var input by rememberSaveable { mutableStateOf("") }
    val listState = rememberLazyListState()

    LaunchedEffect(messages.size) {
        if (messages.isNotEmpty()) listState.animateScrollToItem(messages.size - 1)
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(statusText(state)) },
                actions = {
                    IconButton(onClick = { viewModel.toggleConnection() }) {
                        Icon(
                            imageVector = if (state is ConnectionState.Off) Icons.Filled.PlayArrow else Icons.Filled.Close,
                            contentDescription = null,
                        )
                    }
                    IconButton(onClick = onOpenSettings) {
                        Icon(imageVector = Icons.Filled.Settings, contentDescription = null)
                    }
                },
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .padding(padding)
                .fillMaxSize(),
        ) {
            LazyColumn(
                state = listState,
                modifier = Modifier
                    .weight(1f)
                    .fillMaxWidth(),
                contentPadding = PaddingValues(12.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                items(messages, key = { it.id }) { message ->
                    MessageBubble(message, onRetry = { viewModel.retry(message.id) })
                }
            }
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                OutlinedTextField(
                    value = input,
                    onValueChange = { input = it },
                    modifier = Modifier.weight(1f),
                    placeholder = { Text("Сообщение") },
                )
                Spacer(Modifier.width(8.dp))
                Button(
                    enabled = input.isNotBlank(),
                    onClick = {
                        viewModel.send(input)
                        input = ""
                    },
                ) { Text("Отправить") }
            }
        }
    }
}

private fun statusText(state: ConnectionState): String = when (state) {
    is ConnectionState.Off -> "ai-talk — отключено"
    is ConnectionState.Waiting -> "ai-talk — ожидание..."
    is ConnectionState.Connected -> "ai-talk — ${state.peer}"
}

@Composable
private fun MessageBubble(message: ChatMessage, onRetry: () -> Unit) {
    val alignment = if (message.fromSelf) Alignment.End else Alignment.Start
    val color = if (message.failed) {
        MaterialTheme.colorScheme.errorContainer
    } else if (message.fromSelf) {
        MaterialTheme.colorScheme.primaryContainer
    } else {
        MaterialTheme.colorScheme.surfaceVariant
    }
    Column(modifier = Modifier.fillMaxWidth(), horizontalAlignment = alignment) {
        Surface(color = color, shape = MaterialTheme.shapes.medium) {
            Text(text = message.text, modifier = Modifier.padding(10.dp))
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            if (message.failed) {
                Text(
                    text = "не доставлено",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(horizontal = 4.dp),
                )
                IconButton(onClick = onRetry, modifier = Modifier.width(32.dp)) {
                    Icon(
                        imageVector = Icons.Filled.Refresh,
                        contentDescription = "Повторить отправку",
                        tint = MaterialTheme.colorScheme.error,
                    )
                }
            } else {
                Text(
                    text = DateFormat.format("HH:mm", message.timestamp).toString(),
                    style = MaterialTheme.typography.labelSmall,
                    modifier = Modifier.padding(horizontal = 4.dp),
                )
            }
        }
    }
}
