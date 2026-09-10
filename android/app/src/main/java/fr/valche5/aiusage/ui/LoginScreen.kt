package fr.valche5.aiusage.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.OutlinedTextFieldDefaults
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import fr.valche5.aiusage.UiState

@Composable
fun LoginScreen(state: UiState, onConnect: (token: String, url: String, remember: Boolean) -> Unit) {
    var url by rememberSaveable(state.baseUrl) { mutableStateOf(state.baseUrl) }
    var token by rememberSaveable(state.token) { mutableStateOf(state.token) }
    var remember by rememberSaveable(state.remember) { mutableStateOf(state.remember) }
    val fieldColors = OutlinedTextFieldDefaults.colors(
        focusedBorderColor = Accent,
        unfocusedBorderColor = Line,
        focusedTextColor = Text,
        unfocusedTextColor = Text,
        cursorColor = Accent,
        focusedLabelColor = Muted,
        unfocusedLabelColor = Muted,
    )

    Column(
        modifier = Modifier
            .fillMaxSize()
            .padding(28.dp),
        verticalArrangement = Arrangement.Center,
    ) {
        Text("ai-usage", fontSize = 28.sp, fontWeight = FontWeight.Bold, color = Text)
        Text("Abonnements IA · homelab", color = Muted, modifier = Modifier.padding(top = 4.dp, bottom = 22.dp))
        if (!state.error.isNullOrBlank()) {
            Text(state.error, color = Bad, modifier = Modifier.padding(bottom = 14.dp))
        }
        OutlinedTextField(
            value = url,
            onValueChange = { url = it },
            label = { Text("Serveur") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
            colors = fieldColors,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next),
        )
        Spacer(Modifier.height(12.dp))
        OutlinedTextField(
            value = token,
            onValueChange = { token = it },
            label = { Text("Jeton Bearer") },
            singleLine = true,
            visualTransformation = PasswordVisualTransformation(),
            modifier = Modifier.fillMaxWidth(),
            colors = fieldColors,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, imeAction = ImeAction.Done),
            keyboardActions = KeyboardActions(
                onDone = { if (token.isNotBlank() && !state.loading) onConnect(token, url, remember) },
            ),
        )
        Text(
            "AI_USAGE_API_TOKEN, ou le mot de passe web",
            color = Muted,
            fontSize = 13.sp,
            modifier = Modifier.padding(top = 6.dp),
        )
        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(top = 4.dp)) {
            Checkbox(checked = remember, onCheckedChange = { remember = it })
            Text("Mémoriser le jeton", color = Muted)
        }
        Spacer(Modifier.height(16.dp))
        Button(
            onClick = { onConnect(token, url, remember) },
            enabled = token.isNotBlank() && url.isNotBlank() && !state.loading,
            modifier = Modifier.fillMaxWidth().height(48.dp),
            colors = ButtonDefaults.buttonColors(containerColor = GreenBtn, contentColor = androidx.compose.ui.graphics.Color.White),
        ) {
            if (state.loading) {
                CircularProgressIndicator(color = androidx.compose.ui.graphics.Color.White, strokeWidth = 2.dp, modifier = Modifier.height(22.dp))
            } else {
                Text("Connexion", fontWeight = FontWeight.Bold)
            }
        }
    }
}
