package xyz.sandoxed.focusgate

import android.content.Context
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.WindowInsetsSides
import androidx.compose.foundation.layout.only
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Shapes
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import java.util.Locale

internal enum class PhoneAppearance(val label: String) {
    ORIGINAL("Original"),
    MINIMALIST("Minimalist"),
}

/** The presentation preference belongs to the phone and is available before opening an inbox. */
internal class AppearancePreferences(context: Context) {
    private val prefs = context.getSharedPreferences("appearance", Context.MODE_PRIVATE)

    fun read(): PhoneAppearance =
        if (prefs.getBoolean("minimalist", false)) PhoneAppearance.MINIMALIST else PhoneAppearance.ORIGINAL

    fun save(appearance: PhoneAppearance) {
        prefs.edit().putBoolean("minimalist", appearance == PhoneAppearance.MINIMALIST).apply()
    }
}

// Match the dashboard palette for the native screens that are available offline.
private val paper = Color(0xFFF7F6EE)
private val kraft = Color(0xFFEFE4D2)
private val lime = Color(0xFFD0F030)
private val tape = Color(0xFFFFD91A)
private val signal = Color(0xFFF04400)
private val signalText = Color(0xFFAD3200)
private val ink = Color.Black
private val line = Color.Black
private val white = Color.White
private val corner = RoundedCornerShape(2.dp)

internal val originalColors = lightColorScheme(
    primary = signalText, onPrimary = white, primaryContainer = tape, onPrimaryContainer = ink,
    secondary = ink, secondaryContainer = tape, onSecondaryContainer = ink,
    background = paper, onBackground = ink, surface = paper, onSurface = ink,
    surfaceVariant = white, onSurfaceVariant = line, outline = line,
    surfaceContainerLow = white, surfaceContainer = paper, surfaceContainerHigh = white,
)

internal val originalShapes = Shapes(
    extraSmall = corner, small = corner, medium = corner, large = corner, extraLarge = corner,
)

@Composable
internal fun OriginalHeader(tab: String, state: PhoneState?, selectTab: (String) -> Unit) {
    Column(Modifier.fillMaxWidth().background(signal)
        .windowInsetsPadding(WindowInsets.safeDrawing.only(WindowInsetsSides.Top + WindowInsetsSides.Horizontal))) {
        Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 12.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("FOCUS GATE", fontSize = 25.sp, fontWeight = FontWeight.Black, letterSpacing = 0.8.sp)
                Text("NOTIFICATION INBOX", color = ink, style = MaterialTheme.typography.labelMedium,
                    fontWeight = FontWeight.Bold)
            }
            val status = when {
                state == null -> null
                state.settings.offline -> "OFFLINE"
                state.legacyView -> "PREVIOUS INBOX"
                state.syncStatus == "Synced" -> "SYNCED"
                state.email != null -> "SIGNED IN"
                else -> "LOCAL PROFILE"
            }
            if (status != null) Surface(color = lime, contentColor = ink, shape = corner, border = BorderStroke(1.dp, ink)) {
                Text(status, Modifier.padding(horizontal = 8.dp, vertical = 4.dp),
                    style = MaterialTheme.typography.labelSmall, fontWeight = FontWeight.Bold)
            }
        }
        HorizontalDivider(color = line)
        Row(Modifier.fillMaxWidth().height(IntrinsicSize.Min).selectableGroup()) {
            listOf("Inbox" to "QUEUE & FEED", "Feeds" to "FEEDS", "Settings" to "SETTINGS").forEach { (id, label) ->
                Text(label, modifier = Modifier.weight(1f).heightIn(min = 48.dp).then(
                    Modifier.background(if (id == tab) tape else paper)
                        .selectable(id == tab, role = Role.Tab, onClick = { selectTab(id) })
                        .padding(vertical = 13.dp, horizontal = 4.dp)
                ), textAlign = TextAlign.Center, fontWeight = FontWeight.Bold, style = MaterialTheme.typography.labelLarge)
            }
        }
        HorizontalDivider(color = line)
    }
}

@Composable
internal fun OriginalPager(vm: PhoneViewModel, state: PhoneState) {
    val mode = state.settings.mode
    val online = !state.settings.offline && !state.legacyView && !vm.refreshing
    Column(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 12.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Column(Modifier.fillMaxWidth().border(1.dp, line, corner).background(kraft, corner).padding(10.dp),
            verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text("RETRO PAGER", fontWeight = FontWeight.Black, style = MaterialTheme.typography.titleMedium)
            Text("MODE: " + focusLabel(mode).uppercase(Locale.ROOT),
                modifier = Modifier.fillMaxWidth().background(paper).padding(5.dp), textAlign = TextAlign.Center,
                style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.Bold)
            Row(Modifier.fillMaxWidth().selectableGroup().height(IntrinsicSize.Min),
                horizontalArrangement = Arrangement.spacedBy(5.dp)) {
                listOf("normal", "deep_work", "dnd").forEach { key ->
                    OriginalControl(focusLabel(key).uppercase(Locale.ROOT), selected = mode == key,
                        modifier = Modifier.weight(1f)) { vm.settings { it.copy(mode = key) } }
                }
            }
        }
        Row(Modifier.fillMaxWidth().height(IntrinsicSize.Min), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OriginalControl(if (vm.refreshing) "WORKING…" else "CELLULAR FETCH",
                modifier = Modifier.weight(1f), enabled = online, emphasis = true, onClick = vm::refresh)
            OriginalControl("SYNC ACCOUNT", modifier = Modifier.weight(1f),
                enabled = online && vm.cloudConfigured && state.email != null, onClick = vm::syncAccount)
        }
    }
}

@Composable
private fun OriginalControl(
    label: String,
    modifier: Modifier,
    selected: Boolean? = null,
    enabled: Boolean = true,
    emphasis: Boolean = false,
    onClick: () -> Unit,
) {
    val interaction = if (selected != null) Modifier.selectable(selected, role = Role.RadioButton, onClick = onClick)
        else Modifier.clickable(enabled = enabled, role = Role.Button, onClick = onClick)
    Row(modifier.heightIn(min = 48.dp).background(if (enabled && (emphasis || selected == true)) tape else paper, corner)
        .border(1.dp, if (enabled) line else line.copy(alpha = 0.35f), corner).then(interaction).padding(6.dp),
        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.Center) {
        Text(label, color = if (enabled) ink else Color(0xFF65615B), textAlign = TextAlign.Center,
            fontWeight = FontWeight.Bold, style = MaterialTheme.typography.labelMedium)
    }
}

@Composable
internal fun OriginalInboxItem(item: Item, source: String?, onOpen: () -> Unit, onRead: () -> Unit,
                               onOpenLink: () -> Unit, online: Boolean, canUpdate: Boolean) {
    Surface(onClick = onOpen, modifier = Modifier.fillMaxWidth(), color = white,
        contentColor = ink, shape = corner, border = BorderStroke(1.dp, line)) {
        Column(Modifier.padding(start = 12.dp, top = 10.dp, end = 12.dp)) {
            Text(item.title.ifBlank { "Untitled item" }, fontWeight = FontWeight.Bold,
                style = MaterialTheme.typography.titleMedium)
            if (item.snippet.isNotBlank()) Text(item.snippet, Modifier.padding(top = 4.dp),
                maxLines = 2, overflow = TextOverflow.Ellipsis)
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                Text(source ?: "RSS", Modifier.weight(1f), color = signalText,
                    maxLines = 1, overflow = TextOverflow.Ellipsis, style = MaterialTheme.typography.labelMedium)
                if (item.status == 0 && canUpdate) TextButton(onClick = onRead) { Text("MARK READ") }
                if (item.url.isNotBlank()) TextButton(onClick = onOpenLink, enabled = online) { Text("OPEN LINK") }
            }
        }
    }
}
