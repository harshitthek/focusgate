package xyz.sandoxed.focusgate

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp

@Composable
internal fun SettingToggle(title: String, description: String, enabled: Boolean, change: (Boolean) -> Unit) {
    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.titleSmall)
            Text(description, style = MaterialTheme.typography.bodySmall)
        }
        Switch(checked = enabled, onCheckedChange = change)
    }
}

@Composable
internal fun Feeds(vm: PhoneViewModel, state: PhoneState) {
    var editing by remember { mutableStateOf<Feed?>(null) }
    var removing by remember { mutableStateOf<Feed?>(null) }
    if (state.legacyView) {
        Text("The previous inbox is a local archive.", Modifier.padding(20.dp))
        return
    }
    LazyColumn(modifier = Modifier.padding(horizontal = 20.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
        item {
            Button(onClick = {editing = Feed("", "", "", false, false)}, enabled = state.feeds.size < 8) { Text("Add feed") }
        }
        items(state.conflicts, key = { "conflict-" + it.id }) { feed ->
            Card(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(14.dp)) {
                    Text("Could not sync an edit to ${feed.name}.")
                    state.feeds.firstOrNull { it.id == feed.id }?.let {
                        Text("Current subscription: ${it.name}")
                    }
                    Row {
                        TextButton({ editing = feed }) { Text("Review my edit") }
                        TextButton({ vm.dismiss(feed) }) { Text("Dismiss") }
                    }
                }
            }
        }
        items(state.feeds, key = { it.id }) { feed ->
            Card(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(5.dp)) {
                    Text(feed.name, style = MaterialTheme.typography.titleMedium)
                    Text(feed.url, style = MaterialTheme.typography.bodySmall)
                    val status = when {
                        feed.paused -> "Paused"
                        feed.refreshed == 0L -> "Awaiting first refresh"
                        else -> "Refreshed " + stamp(feed.refreshed)
                    }
                    Text(status, style = MaterialTheme.typography.labelMedium)
                    Row {
                        TextButton({editing = feed}) { Text("Edit") }
                        TextButton({vm.save(feed.copy(paused = !feed.paused))}) { Text(if (feed.paused) "Resume" else "Pause") }
                        TextButton({removing = feed}) { Text("Remove") }
                    }
                }
            }
        }
    }
    editing?.let { feed ->
        val reAdd = feed.id.isNotEmpty() && !feed.removed && state.feeds.none { it.id == feed.id }
        FeedDialog(feed, state.email != null && vm.assessmentConfigured, reAdd, onClose = {editing = null}) {
            vm.save(it)
            editing = null
        }
    }
    removing?.let { feed ->
        AlertDialog(onDismissRequest = {removing = null}, title = {Text("Remove ${feed.name}?")},
            text = {Text("Existing items remain in the inbox.")},
            confirmButton = {Button({vm.save(feed.copy(removed = true)); removing = null}) {Text("Remove")}},
            dismissButton = {TextButton({removing = null}) {Text("Cancel")}})
    }
}

@Composable
private fun FeedDialog(feed: Feed, assessment: Boolean, reAdd: Boolean, onClose: () -> Unit, onSave: (Feed) -> Unit) {
    var name by rememberSaveable(feed.id) { mutableStateOf(feed.name) }
    var url by rememberSaveable(feed.id) { mutableStateOf(feed.url) }
    var assess by rememberSaveable(feed.id) { mutableStateOf(feed.assess) }
    AlertDialog(onDismissRequest = onClose, title = {Text(if (feed.id.isEmpty()) "Add RSS or Atom feed" else "Edit feed")},
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(name, {name = it}, label = { Text("Name (optional)") }, singleLine = true)
                OutlinedTextField(url, {url = it}, label = { Text("Feed URL") }, singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                    enabled = feed.id.isEmpty())
                if (assessment) SettingToggle("Remote priority assessment",
                    "Send the title and a short excerpt of ambiguous items to FocusGate’s assessment service.", assess) {
                    assess = it
                }
            }
        },
        confirmButton = {Button(onClick = {onSave(feed.copy(name = name, url = url, assess = assess))}, enabled = url.isNotBlank()) {
            Text(if (reAdd) "Re-add feed" else if (feed.removed) "Remove feed" else "Save")
        }},
        dismissButton = {TextButton(onClose) {Text("Cancel")}})
}

@Composable
internal fun AccountDialog(vm: PhoneViewModel, onClose: () -> Unit) {
    var action by rememberSaveable { mutableStateOf("login") }
    var email by rememberSaveable { mutableStateOf("") }
    var name by rememberSaveable { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var code by remember { mutableStateOf("") }
    val title = when (action) {
        "create" -> "Create account"
        "verify" -> "Verify email"
        else -> "Sign in"
    }
    AlertDialog(onDismissRequest = onClose, title = {Text(title)},
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                vm.error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
                OutlinedTextField(email, {email = it}, label = {Text("Email")}, singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email))
                if (action == "create") OutlinedTextField(name, {name = it}, label = {Text("Name")}, singleLine = true)
                if (action != "verify") OutlinedTextField(password, {password = it}, label = {Text("Password")},
                    singleLine = true, visualTransformation = PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password))
                else OutlinedTextField(code, {code = it}, label = {Text("Email code")}, singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number))
                if (action == "login") {
                    TextButton({ action = "create" }) {Text("Create an account")}
                    TextButton({ action = "verify" }) {Text("Verify email")}
                } else if (action == "verify") {
                    TextButton(enabled = email.isNotBlank() && !vm.refreshing, onClick = {
                        vm.authenticate("send", email, "", "", "") {}
                    }) {Text("Send verification code")}
                    TextButton({action = "login"}) {Text("Return to sign in")}
                } else TextButton({action = "login"}) {Text("Return to sign in")}
            }
        },
        confirmButton = {
            val valid = email.isNotBlank() && when (action) {
                "verify" -> code.isNotBlank()
                "create" -> name.isNotBlank() && password.length >= 8
                else -> password.isNotBlank()
            }
            Button(enabled = valid && !vm.refreshing, onClick = {
                vm.authenticate(action, email, password, name, code) {
                    when (action) {
                        "create" -> { action = "verify"; password = "" }
                        "verify" -> { action = "login"; code = "" }
                        else -> onClose()
                    }
                }
            }) { Text(if (vm.refreshing) "Please wait…" else title) }
        },
        dismissButton = {TextButton(onClose) {Text("Cancel")}})
}
