package xyz.sandoxed.focusgate

import android.Manifest
import android.app.Application
import android.content.Intent
import android.content.ActivityNotFoundException
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import androidx.core.view.WindowCompat
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Shapes
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.text.DateFormat
import java.util.Date

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            val vm: PhoneViewModel = viewModel()
            val original = vm.appearance == PhoneAppearance.ORIGINAL
            val dark = !original && isSystemInDarkTheme()
            val colors = if (dark) darkColorScheme(
                primary = Color(0xFF9CD9BE), background = Color(0xFF121915), surface = Color(0xFF161E19),
                surfaceVariant = Color(0xFF24362D),
            ) else lightColorScheme(
                primary = Color(0xFF285844), background = Color(0xFFF8F8F1), surface = Color(0xFFF8F8F1),
                surfaceVariant = Color(0xFFE5ECE1),
            )
            SideEffect {
                WindowCompat.getInsetsController(window, window.decorView).apply {
                    isAppearanceLightStatusBars = !dark
                    isAppearanceLightNavigationBars = !dark
                }
            }
            MaterialTheme(colorScheme = if (original) originalColors else colors,
                shapes = if (original) originalShapes else Shapes()) {
                FocusGate(vm, original)
            }
        }
    }
}

internal class PhoneViewModel(app: Application) : AndroidViewModel(app) {
    private val phone by lazy { PhoneRepository.get(app) }
    private val appearancePreferences = AppearancePreferences(app)
    var appearance by mutableStateOf(appearancePreferences.read())
        private set
    val state = MutableStateFlow<PhoneState?>(null)
    var error by mutableStateOf<String?>(null)
    var refreshing by mutableStateOf(false)
    var archive by mutableStateOf(false)
        private set
    var digest by mutableStateOf(false)
        private set
    private var pages = 1
    private var loadId = 0

    val cloudConfigured get() = BuildConfig.NEON_AUTH_URL.isNotBlank() && BuildConfig.SYNC_URL.isNotBlank()
    val assessmentConfigured get() = BuildConfig.ASSESSMENT_URL.isNotBlank()

    init {
        local { phone.start() }
        viewModelScope.launch {
            try {
                val updates = withContext(Dispatchers.IO) { phone.updates }
                updates.collect { reload() }
            } catch (e: CancellationException) {
                throw e
            } catch (_: Exception) {
                error = "Could not open the on-device inbox"
            }
        }
    }

    fun showArchive(show: Boolean) {
        archive = show
        digest = false
        pages = 1
        reload()
    }

    fun showDigest() {
        archive = false
        digest = true
        pages = 1
        reload()
    }

    fun more() { pages++; reload() }
    fun save(feed: Feed) = local { phone.saveFeed(feed) }
    fun dismiss(feed: Feed) = local { phone.dismissConflict(feed) }
    fun mark(item: Item, status: Int) {
        val serverId = item.dashboardId
        if (serverId == null) local { phone.mark(item.id, status) }
        else remote { phone.markDashboard(serverId, status) }
    }
    fun settings(settings: PhoneSettings) = local { phone.setSettings(settings) }
    fun importLocal(import: Boolean) = local { phone.resolveImport(import) }
    fun legacy(show: Boolean) = local { phone.showLegacy(show) }
    fun permissionChanged() = local { phone.deliver() }
    fun refresh() = remote { phone.refresh(fetchFeeds = true) }
    fun syncAccount() = remote { phone.refresh(fetchFeeds = false) }
    fun refreshDashboard() = remote { phone.refreshDashboard() }
    suspend fun claimDashboard() = withContext(Dispatchers.IO) { phone.claimDashboard() }
    fun forgetDashboard() = local { phone.forgetDashboard() }
    fun chooseAppearance(next: PhoneAppearance) {
        appearancePreferences.save(next)
        appearance = next
    }
    fun signOut() = local { phone.signOut() }
    fun authenticate(action: String, email: String, password: String, name: String, code: String, done: () -> Unit) =
        remote(done) {
            if (action == "login") phone.signIn(email.trim(), password)
            else phone.authAction(action, email.trim(), password, name.trim(), code.trim())
        }

    private fun run(done: (() -> Unit)? = null, busy: Boolean = false, action: suspend () -> Unit) {
        viewModelScope.launch {
            error = null
            try {
                withContext(Dispatchers.IO) { action() }
                done?.invoke()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                error = e.message?.take(240) ?: "The action could not be completed"
            } finally {
                if (busy) refreshing = false
                reload()
            }
        }
    }

    private fun local(action: suspend () -> Unit) = run(action = action)
    private fun remote(done: (() -> Unit)? = null, action: suspend () -> Unit) {
        if (refreshing) return
        refreshing = true
        run(done, true, action)
    }

    fun reload() {
        val n = ++loadId
        val showArchive = archive
        val showDigest = digest
        val limit = pages
        viewModelScope.launch {
            try {
                val latest = withContext(Dispatchers.IO) { phone.snapshot(showArchive, limit, showDigest) }
                if (n == loadId) state.value = latest
            } catch (e: CancellationException) {
                throw e
            } catch (_: Exception) {
                if (n == loadId) error = "Could not open the on-device inbox"
            }
        }
    }
}

@Composable
private fun FocusGate(vm: PhoneViewModel, original: Boolean) {
    val state by vm.state.collectAsState()
    var tab by rememberSaveable { mutableStateOf("Inbox") }
    var dashboardOverride by rememberSaveable { mutableStateOf<Boolean?>(null) }
    val dashboardPossible = state?.let { !it.settings.offline && !it.legacyView } == true
    val showDashboard = dashboardPossible && tab == "Inbox" && (dashboardOverride ?: original)
    val context = LocalContext.current
    var canNotify by remember {
        mutableStateOf(Build.VERSION.SDK_INT < 33 ||
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED)
    }
    val permission = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {
        canNotify = it
        vm.permissionChanged()
    }
    val selectTab: (String) -> Unit = { selected ->
        if (showDashboard && selected != "Inbox") vm.refreshDashboard()
        tab = selected
        vm.reload()
    }
    val showPhone: () -> Unit = {
        dashboardOverride = false
        vm.refreshDashboard()
    }
    BackHandler(enabled = showDashboard) { showPhone() }
    Scaffold(topBar = {
        if (showDashboard) {
            Row(Modifier.fillMaxWidth().statusBarsPadding().padding(horizontal = 8.dp),
                horizontalArrangement = Arrangement.SpaceBetween, verticalAlignment = Alignment.CenterVertically) {
                TextButton(showPhone) { Text("Saved inbox") }
                TextButton({ selectTab("Feeds") }) { Text("Feeds") }
                TextButton({ selectTab("Settings") }) { Text("Settings") }
            }
        } else if (original) OriginalHeader(tab, state, selectTab)
    }, bottomBar = {
        if (!original && !showDashboard) {
            NavigationBar {
                listOf("Inbox" to "▤", "Feeds" to "≋", "Settings" to "⚙").forEach { (label, icon) ->
                    NavigationBarItem(selected = tab == label, onClick = { selectTab(label) },
                        label = { Text(label) }, icon = { Text(icon, style = MaterialTheme.typography.titleLarge) })
                }
            }
        }
    }) { insets ->
        val current = state
        Column(Modifier.fillMaxSize().padding(insets)) {
            if (showDashboard && current != null) {
                key(current.dashboardProfile) { DashboardView(vm, current, showPhone) }
                return@Column
            }
            if (!original) Row(Modifier.fillMaxWidth().padding(horizontal = 20.dp, vertical = 14.dp),
                horizontalArrangement = Arrangement.SpaceBetween, verticalAlignment = Alignment.CenterVertically) {
                Text(tab, style = MaterialTheme.typography.headlineLarge)
                state?.let {
                    Text(if (it.settings.offline) "Offline mode" else focusLabel(it.settings.mode),
                        color = MaterialTheme.colorScheme.primary, style = MaterialTheme.typography.labelLarge)
                }
            }
            vm.error?.let { Text(it, Modifier.padding(horizontal = 20.dp, vertical = 6.dp), color = MaterialTheme.colorScheme.error) }
            if (current == null) {
                Text(if (vm.error == null) "Opening inbox…" else "Tap a tab to retry.", Modifier.padding(20.dp))
            } else {
                when (tab) {
                    "Inbox" -> Inbox(vm, current, original, { selectTab("Feeds") }, {
                        dashboardOverride = true
                    })
                    "Feeds" -> Feeds(vm, current)
                    else -> Settings(vm, current, canNotify,
                        chooseAppearance = { choice ->
                            dashboardOverride = null
                            vm.chooseAppearance(choice)
                        },
                        showDashboard = {
                            dashboardOverride = true
                            selectTab("Inbox")
                        },
                        requestPermission = {
                            if (Build.VERSION.SDK_INT >= 33) permission.launch(Manifest.permission.POST_NOTIFICATIONS)
                        })
                }
            }
        }
    }
}

@Composable
private fun Inbox(vm: PhoneViewModel, state: PhoneState, original: Boolean, showFeeds: () -> Unit,
                  showDashboard: () -> Unit) {
    var selectedId by rememberSaveable { mutableStateOf<String?>(null) }
    var source by rememberSaveable { mutableStateOf("all") }
    var query by rememberSaveable { mutableStateOf("") }
    val context = LocalContext.current
    if (original) OriginalPager(vm, state)
    Row(Modifier.fillMaxWidth().padding(horizontal = 20.dp), horizontalArrangement = Arrangement.spacedBy(8.dp),
        verticalAlignment = Alignment.CenterVertically) {
        FilterChip(!vm.archive && !vm.digest, { vm.showArchive(false) }, { Text("Active") })
        FilterChip(vm.archive, { vm.showArchive(true) }, { Text("Archive") })
        FilterChip(vm.digest, { vm.showDigest() }, { Text("Digest") })
        Box(Modifier.weight(1f))
        if (!original) TextButton(onClick = vm::refresh, enabled = !state.settings.offline && !vm.refreshing && !state.legacyView) {
            Text(if (vm.refreshing) "Refreshing…" else "Refresh")
        }
    }
    val sources = state.items.map { it.source }.distinct()
    val localQuery = query.trim()
    val visible = state.items.filter { item ->
        (source == "all" || source == item.source) && (localQuery.isEmpty() ||
            item.title.contains(localQuery, true) || item.snippet.contains(localQuery, true) ||
            item.sourceName.contains(localQuery, true) || item.sender.contains(localQuery, true))
    }
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
        if (!state.settings.offline && !state.legacyView) item {
            TextButton(showDashboard) { Text("Dashboard and connected sources") }
        }
        if (state.items.isNotEmpty()) item {
            OutlinedTextField(query, { query = it }, Modifier.fillMaxWidth(),
                label = { Text("Search inbox") }, singleLine = true)
            LazyRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                items(listOf("all") + sources) { key ->
                    FilterChip(source == key, { source = key }, { Text(sourceLabel(key)) })
                }
            }
        }
        if (state.items.isEmpty()) item {
            Text(if (vm.archive) "Archived items appear here."
                else if (vm.digest) "There are no unread digest items."
                else if (state.feeds.isEmpty()) "Sign in to the dashboard to read connected notifications, or add a feed on this phone."
                else "Your inbox is clear.")
            if (!vm.archive && state.feeds.isEmpty() && !state.legacyView) TextButton(showFeeds) { Text("Add a feed") }
        }
        if (state.items.isNotEmpty() && visible.isEmpty()) item { Text("No matching notifications.") }
        items(visible, key = { it.id }) { item ->
            val sourceName = if (item.dashboardId == null) state.feeds.firstOrNull { it.id == item.feed }?.name
                else listOf(sourceLabel(item.source), item.sourceName).filter { it.isNotBlank() }.distinct().joinToString(" · ")
            if (original) {
                OriginalInboxItem(item, sourceName, onOpen = { selectedId = item.id }, onRead = { vm.mark(item, 1) },
                    onOpenLink = { openArticle(context, item.url) { vm.error = "No browser is available" } },
                    online = !state.settings.offline, canUpdate = item.dashboardId == null || !state.settings.offline)
            } else Card(onClick = { selectedId = item.id }, modifier = Modifier.fillMaxWidth()) {
                Column(Modifier.padding(15.dp), verticalArrangement = Arrangement.spacedBy(5.dp)) {
                    if (sourceName != null || item.priority >= 4) {
                        Text(listOfNotNull(sourceName, "High priority".takeIf { item.priority >= 4 }).joinToString(" · "),
                            style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
                    }
                    Text(item.title.ifBlank { "Untitled item" }, style = MaterialTheme.typography.titleMedium)
                    if (item.snippet.isNotBlank()) Text(item.snippet, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    Text(stamp(item.published), style = MaterialTheme.typography.labelSmall)
                }
            }
        }
        if (state.hasMore) item { TextButton(onClick = vm::more) { Text("Load more") } }
    }
    state.items.firstOrNull { it.id == selectedId }?.let { item ->
        val canUpdate = item.dashboardId == null || !state.settings.offline
        AlertDialog(onDismissRequest = {selectedId = null}, title = { Text(item.title.ifBlank { "Untitled item" }) },
            text = {
                Column(Modifier.verticalScroll(rememberScrollState())) {
                    if (item.sender.isNotBlank()) Text(item.sender, style = MaterialTheme.typography.labelLarge)
                    if (item.snippet.isNotBlank()) Text(item.snippet)
                    if (item.url.isNotBlank()) TextButton(enabled = !state.settings.offline, onClick = {
                        openArticle(context, item.url) { vm.error = "No browser is available" }
                    }) { Text("Open article") }
                }
            },
            dismissButton = {
                TextButton(onClick = {
                    selectedId = null
                    if (item.status == 0 && canUpdate) vm.mark(item, 1)
                }) { Text(if (item.status == 0 && canUpdate) "Mark read" else "Close") }
            },
            confirmButton = {
                if (item.status != 2 && canUpdate) Button(onClick = { selectedId = null; vm.mark(item, 2) }) { Text("Archive") }
            })
    }
}

internal fun openArticle(context: android.content.Context, url: String, onUnavailable: () -> Unit) {
    val uri = Uri.parse(url)
    if (uri.scheme in listOf("https", "http") && !uri.host.isNullOrBlank()) {
        try { context.startActivity(Intent(Intent.ACTION_VIEW, uri)) }
        catch (_: ActivityNotFoundException) { onUnavailable() }
    }
}

@Composable
private fun Settings(vm: PhoneViewModel, state: PhoneState, canNotify: Boolean,
                     chooseAppearance: (PhoneAppearance) -> Unit, showDashboard: () -> Unit,
                     requestPermission: () -> Unit) {
    var auth by rememberSaveable { mutableStateOf(false) }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(20.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("Appearance", style = MaterialTheme.typography.titleMedium)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            PhoneAppearance.entries.forEach { appearance ->
                FilterChip(
                    selected = vm.appearance == appearance,
                    onClick = { chooseAppearance(appearance) },
                    label = { Text(appearance.label) },
                )
            }
        }
        HorizontalDivider()
        Text("Connected sources", style = MaterialTheme.typography.titleMedium)
        Text("Read GitHub, email, Slack, webhooks and feeds together. Connect and manage sources in the FocusGate dashboard.")
        OutlinedButton(showDashboard, enabled = !state.settings.offline && !state.legacyView) { Text("Open dashboard") }
        HorizontalDivider()
        UpdateSettings(state.settings.offline)
        HorizontalDivider()
        Text("Focus mode", style = MaterialTheme.typography.titleMedium)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            listOf("normal", "deep_work", "dnd").forEach { key ->
                FilterChip(state.settings.mode == key, { vm.settings(state.settings.copy(mode = key)) },
                    { Text(focusLabel(key)) })
            }
        }
        SettingToggle("Offline mode", "Pause dashboard, feed, account, sync and update requests.", state.settings.offline) {
            vm.settings(state.settings.copy(offline = it))
        }
        SettingToggle("Feed alerts", "Allow priority alerts and local digests.", state.settings.alerts) {
            vm.settings(state.settings.copy(alerts = it))
            if (it && !canNotify) requestPermission()
        }
        if (state.settings.alerts && !canNotify) {
            TextButton(requestPermission) { Text("Allow Android notifications") }
        }
        HorizontalDivider()
        Text("Account", style = MaterialTheme.typography.titleMedium)
        when {
            state.legacyView -> {
                Text("Previous FocusGate inbox")
                OutlinedButton({ vm.legacy(false) }) { Text("Return to local profile") }
            }
            state.email != null -> {
                Text(state.email)
                state.syncStatus?.let { Text(it) }
                if (state.syncStatus == "Sign in again to resume sync") {
                    OutlinedButton({ auth = true }, enabled = !state.settings.offline) { Text("Sign in again") }
                }
                OutlinedButton(vm::signOut) { Text("Sign out") }
            }
            else -> {
                Text("Local profile")
                if (vm.cloudConfigured) Button(onClick = { auth = true }, enabled = !state.settings.offline) { Text("Sign in to sync") }
                else Text("Cloud sync is unavailable in this build.")
                if (state.legacyAvailable) OutlinedButton({ vm.legacy(true) }) { Text("Open previous inbox") }
            }
        }
    }
    if (auth && !state.settings.offline && !state.legacyView) AccountDialog(vm) { auth = false }
    if (state.importOffered) AlertDialog(onDismissRequest = { vm.importLocal(false) },
        title = { Text("Import local inbox?") },
        text = { Text("Copy this phone’s feeds and inbox state into your signed-in profile for sync.") },
        confirmButton = { Button({vm.importLocal(true)}) { Text("Import and sync") } },
        dismissButton = { TextButton({vm.importLocal(false)}) { Text("Keep separate") } })
}

internal fun focusLabel(mode: String) = when (mode) {
    "deep_work" -> "Deep work"
    "dnd" -> "DND"
    else -> "Normal"
}

private fun sourceLabel(source: String) = when (source) {
    "all" -> "All sources"
    "rss" -> "RSS"
    "github" -> "GitHub"
    "imap" -> "Email"
    "slack" -> "Slack"
    "webhook" -> "Webhook"
    else -> source
}

internal fun stamp(unix: Long): String =
    DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(unix * 1000))
