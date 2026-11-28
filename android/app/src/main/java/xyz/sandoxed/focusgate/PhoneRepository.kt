package xyz.sandoxed.focusgate

import android.Manifest
import android.annotation.SuppressLint
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.SystemClock
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import androidbridge.Androidbridge
import androidbridge.Client
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.job
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.update
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.io.IOException
import java.util.TimeZone
import java.util.UUID
import java.util.concurrent.Executors
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

internal data class PhoneSettings(val offline: Boolean, val alerts: Boolean, val mode: String) {
    fun json() = JSONObject().put("offline", offline).put("alerts", alerts).put("mode", mode)
}

internal data class Feed(
    val id: String, val url: String, val name: String,
    val paused: Boolean, val assess: Boolean, val refreshed: Long = 0, val removed: Boolean = false,
) {
    fun json() = JSONObject().put("id", id).put("url", url).put("name", name)
        .put("paused", paused).put("assess", assess).put("removed", removed)

    companion object {
        fun read(o: JSONObject) = Feed(o.getString("id"), o.getString("url"), o.getString("name"),
            o.getBoolean("paused"), o.getBoolean("assess"), o.getLong("refreshed"), o.optBoolean("removed"))
    }
}

internal data class Item(val id: String, val title: String, val snippet: String, val url: String,
                         val priority: Int, val status: Int, val published: Long, val feed: String,
                         val source: String = "rss", val sourceName: String = "", val dashboardId: Long? = null,
                         val digested: Boolean = false, val sender: String = "")

internal data class PhoneState(
    val settings: PhoneSettings, val feeds: List<Feed>, val items: List<Item>,
    val conflicts: List<Feed>, val email: String?, val syncStatus: String?,
    val importOffered: Boolean, val legacyAvailable: Boolean, val legacyView: Boolean, val hasMore: Boolean,
    val dashboardAllowed: Boolean, val dashboardProfile: String,
)

private fun JSONArray.objects() = List(length()) { index -> getJSONObject(index) }
private fun JSONObject.objects(key: String) = getJSONArray(key).objects()

internal class PhoneRepository private constructor(private val context: Context) {
    private val lock = Any()
    private val dir = File(context.noBackupFilesDir, "phone").absolutePath
    private val vault = SessionVault(context)
    private val auth = NeonAuth()
    private val dashboard = DashboardInbox(context)
    private val prefs = context.getSharedPreferences("phone", Context.MODE_PRIVATE)
    private var account = vault.load()
    private var legacy = prefs.getBoolean("legacy_view", false) && account == null
    private var client = open(account, legacy)
    private var token = ""
    private var tokenUntil = 0L
    private var epoch = 0L
    private var active: Job? = null
    private var syncStatus: String? = null
    val updates = MutableStateFlow(0L)

    private val legacyFile get() = File(context.filesDir, "fyne/focusgate.db")

    private fun open(account: Account?, legacy: Boolean): Client {
        val storage = if (legacy) File(dir, "legacy").absolutePath else dir
        val opened = Androidbridge.open(storage, account?.issuer.orEmpty(), account?.subject.orEmpty())
        try {
            if (legacy) opened.importLegacy(legacyFile.absolutePath)
        } catch (e: Exception) {
            opened.close()
            throw e
        }
        return opened
    }

    private fun settings(): PhoneSettings = JSONObject(client.readSettings()).let {
        PhoneSettings(it.getBoolean("offline"), it.getBoolean("alerts"), it.getString("mode"))
    }

    private fun dashboardProfile() = dashboardProfile(account?.issuer, account?.subject)

    fun claimDashboard(): Boolean = synchronized(lock) {
        !legacy && !settings().offline && dashboard.claim(dashboardProfile())
    }

    fun forgetDashboard() = synchronized(lock) {
        dashboard.forget()
        updates.update { it + 1 }
    }

    fun snapshot(archive: Boolean, pages: Int, digest: Boolean = false): PhoneState = synchronized(lock) {
        val raw = JSONObject(client.state(archive, 0))
        val all = mutableListOf<JSONObject>()
        all.addAll(if (digest) JSONArray(client.digest(0)).objects() else raw.objects("items"))
        repeat((pages - 1).coerceIn(0, 9)) { index ->
            if (all.size == (index + 1) * 100) {
                val offset = (index + 1) * 100L
                all.addAll(if (digest) JSONArray(client.digest(offset)).objects()
                    else JSONObject(client.state(archive, offset)).objects("items"))
            }
        }
        val s = raw.getJSONObject("settings")
        val connected = if (legacy) emptyList() else dashboard.cached(dashboard.access(dashboardProfile()))
            .filter { if (digest) it.digested else (it.status == 2) == archive }
        val local = all.distinctBy { it.getString("id") }.map {
            Item(it.getString("id"), it.getString("title"), it.getString("snippet"), it.getString("url"),
                it.getInt("priority"), it.getInt("status"), it.getLong("published"), it.getString("feedId"))
        }
        PhoneState(PhoneSettings(s.getBoolean("offline"), s.getBoolean("alerts"), s.getString("mode")),
            raw.objects("feeds").map(Feed::read),
            (local + connected).sortedWith(compareByDescending<Item> { it.priority }.thenByDescending { it.published }),
            raw.objects("conflicts").map(Feed::read), account?.email, syncStatus,
            account != null && !prefs.getBoolean(importKey(), false), account == null && legacyFile.exists() && !legacy,
            legacy, all.size == pages * 100 && pages < 10, !legacy && dashboard.belongsTo(dashboardProfile()), dashboardProfile())
    }

    private fun importKey() = "import.${account?.issuer}.${account?.subject}"

    fun resolveImport(import: Boolean) = synchronized(lock) {
        if (account != null) {
            if (import) client.importLocal()
            check(prefs.edit().putBoolean(importKey(), true).commit())
            if (import && !settings().offline) PhoneWork.syncSoon(context)
        }
    }

    fun saveFeed(feed: Feed) = synchronized(lock) {
        check(!legacy) { "Open the local profile to edit subscriptions" }
        client.saveFeed(feed.json().toString())
        if (account != null && !settings().offline) PhoneWork.syncSoon(context)
    }

    fun dismissConflict(feed: Feed) = synchronized(lock) {
        client.dismissConflict(feed.id)
    }

    fun mark(id: String, status: Int) = synchronized(lock) {
        client.mark(id, status.toLong())
        if (account != null && !settings().offline) PhoneWork.syncSoon(context)
    }

    fun setSettings(next: PhoneSettings) = synchronized(lock) {
        val previous = settings()
        client.settings(next.json().toString())
        if (previous.offline != next.offline) {
            if (next.offline) {
                epoch++
                active?.cancel()
                client.stopNetwork()
                PhoneWork.pauseNetwork(context)
            } else {
                PhoneWork.schedule(context, online = !legacy)
                if (!legacy && account != null) PhoneWork.syncSoon(context)
            }
        }
        if (!next.alerts) NotificationManagerCompat.from(context).cancelAll()
        if (next.alerts) deliver()
    }

    fun start() = synchronized(lock) {
        val s = settings()
        PhoneWork.schedule(context, online = !legacy && !s.offline)
        deliver()
    }

    fun showLegacy(show: Boolean) = synchronized(lock) {
        if (account != null || legacy == show) return@synchronized
        replace(null, show, "")
        start()
    }

    suspend fun signIn(email: String, password: String) = network {
        val login = auth.signIn(email, password)
        synchronized(lock) {
            ensureCurrent()
            replace(login.account, false, login.token)
            PhoneWork.schedule(context, online = !settings().offline)
            PhoneWork.syncSoon(context)
        }
    }

    suspend fun authAction(action: String, email: String, password: String, name: String, code: String) = network {
        when (action) {
            "create" -> auth.register(name, email, password)
            "send" -> auth.sendVerification(email)
            "verify" -> auth.verify(email, code)
            else -> error("Unknown account action")
        }
    }

    suspend fun signOut() {
        val revoke = synchronized(lock) {
            val previous = account ?: return
            val offline = settings().offline
            active?.cancel()
            active = null
            replace(null, false, "")
            start()
            if (offline) null else previous.cookie
        }
        if (revoke != null) {
            try {
                network { auth.end(revoke) }
            } catch (e: CancellationException) { throw e } catch (_: Exception) {
                // The device already discarded the credential and locked the account.
            }
        }
    }

    private fun replace(next: Account?, nextLegacy: Boolean, access: String) {
        val previousSettings = settings()
        val nextClient = open(next, nextLegacy)
        try {
            val nextSettings = JSONObject(nextClient.readSettings())
                .put("mode", previousSettings.mode)
                .put("offline", previousSettings.offline)
            nextClient.settings(nextSettings.toString())
            if (next == null) vault.clear() else vault.save(next)
        } catch (e: Exception) {
            nextClient.close()
            throw e
        }
        client.stopNetwork()
        client.close()
        epoch++
        PhoneWork.pauseNetwork(context)
        NotificationManagerCompat.from(context).cancelAll()
        client = nextClient
        account = next
        legacy = nextLegacy
        prefs.edit().putBoolean("legacy_view", nextLegacy).apply()
        token = access
        tokenUntil = if (access.isEmpty()) 0 else SystemClock.elapsedRealtime() + TOKEN_CACHE
        syncStatus = null
        updates.update { it + 1 }
    }

    suspend fun refresh(fetchFeeds: Boolean) = network {
        val signedIn = synchronized(lock) { account }
        var access = ""
        var refreshError: Exception? = null
        if (signedIn != null) {
            access = try {
                accessToken(signedIn)
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                val signedOut = e is AuthError && (e.status == 401 || e.status == 403)
                if (!signedOut && !fetchFeeds) refreshError = e
                synchronized(lock) {
                    ensureCurrent()
                    syncStatus = if (signedOut) {
                        "Sign in again to resume sync"
                    } else "Sync is temporarily unavailable"
                }
                ""
            }
        }
        if (fetchFeeds) {
            try {
                blocking { boundClient.refresh(id, BuildConfig.ASSESSMENT_URL, access) }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                refreshError = e
            }
        }
        try {
            refreshDashboardInbox()
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            if (refreshError == null) refreshError = e
        }
        synchronized(lock) { ensureCurrent(); deliver() }
        if (access.isNotEmpty()) {
            try {
                blocking { boundClient.sync(id, BuildConfig.SYNC_URL, access) }
                synchronized(lock) { ensureCurrent(); syncStatus = "Synced" }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                synchronized(lock) {
                    ensureCurrent()
                    if (e.message?.contains("HTTP 401") == true) {
                        token = ""
                        tokenUntil = 0
                    }
                    syncStatus = "Sync is temporarily unavailable"
                }
                if (!fetchFeeds) throw e
            }
        }
        refreshError?.let { throw it }
    }

    suspend fun refreshDashboard() = network { refreshDashboardInbox() }

    suspend fun markDashboard(id: Long, status: Int) = network {
        val linked = synchronized(lock) {
            ensureCurrent()
            dashboard.access(dashboardProfile()) ?: throw IOException("Sign in to the dashboard to update this item")
        }
        dashboard.mark(linked, id, status)
        refreshDashboardInbox()
    }

    private suspend fun OnlineOperation.refreshDashboardInbox() {
        val linked = synchronized(lock) {
            ensureCurrent()
            dashboard.access(dashboardProfile())
        } ?: return
        val rows = try {
            dashboard.fetch(linked)
        } catch (e: AuthError) {
            synchronized(lock) {
                ensureCurrent()
                if (dashboard.access(dashboardProfile())?.key == linked.key) dashboard.invalidate(linked)
            }
            throw e
        }
        synchronized(lock) {
            ensureCurrent()
            if (dashboard.access(dashboardProfile())?.key == linked.key) dashboard.write(linked, rows)
        }
    }

    private suspend fun OnlineOperation.accessToken(previous: Account): String {
        synchronized(lock) {
            ensureCurrent()
            if (token.isNotEmpty() && SystemClock.elapsedRealtime() < tokenUntil) return token
        }
        val latest = auth.refresh(previous)
        synchronized(lock) {
            ensureCurrent()
            if (latest.account.cookie != previous.cookie) vault.save(latest.account)
            account = latest.account
            token = latest.token
            tokenUntil = SystemClock.elapsedRealtime() + TOKEN_CACHE
            return token
        }
    }

    private inner class OnlineOperation(val version: Long, val boundClient: Client, val id: String) {
        fun ensureCurrent() {
            if (version != epoch || settings().offline) throw CancellationException("profile or offline setting changed")
        }

        suspend fun blocking(call: () -> Unit) = suspendCancellableCoroutine { continuation ->
            continuation.invokeOnCancellation { boundClient.endNetwork(id) }
            networkThreads.execute {
                val result = runCatching(call)
                result.fold(continuation::resume, continuation::resumeWithException)
            }
        }
    }

    private suspend fun <T> network(body: suspend OnlineOperation.() -> T): T = coroutineScope {
        val job = currentCoroutineContext().job
        val operation = synchronized(lock) {
            check(!settings().offline) { "Offline mode is enabled" }
            check(active == null) { "Another network action is in progress" }
            val id = UUID.randomUUID().toString()
            client.beginNetwork(id)
            active = job
            OnlineOperation(epoch, client, id)
        }
        try {
            operation.body()
        } finally {
            operation.boundClient.endNetwork(operation.id)
            synchronized(lock) {
                if (active == job) active = null
            }
        }
    }

    @SuppressLint("MissingPermission")
    fun deliver() = synchronized(lock) {
        if (!NotificationManagerCompat.from(context).areNotificationsEnabled()) return@synchronized
        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return@synchronized
        val name = "focusgate_feed"
        if (Build.VERSION.SDK_INT >= 26) {
            context.getSystemService(NotificationManager::class.java)
                .createNotificationChannel(NotificationChannel(name, "FocusGate feed alerts", NotificationManager.IMPORTANCE_DEFAULT))
        }
        val click = PendingIntent.getActivity(context, 0, Intent(context, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        val zone = TimeZone.getDefault()
        val notices = JSONArray(client.notices(zone.id, zone.getOffset(System.currentTimeMillis()) / 1000L))
        repeat(notices.length()) { index ->
            val notice = notices.getJSONObject(index)
            val alert = NotificationCompat.Builder(context, name)
                .setSmallIcon(R.drawable.ic_alert).setContentTitle(notice.getString("title"))
                .setContentText(notice.getString("body"))
                .setStyle(NotificationCompat.BigTextStyle().bigText(notice.getString("body")))
                .setOnlyAlertOnce(true).setAutoCancel(true).setContentIntent(click)
                .setVisibility(NotificationCompat.VISIBILITY_PRIVATE).build()
            val id = notice.getString("id")
            val tag = if (id == "digest") "digest." + notice.getLong("slot") else id
            NotificationManagerCompat.from(context).notify(tag, 1, alert)
            client.acknowledge(notice.toString())
        }
    }

    companion object {
        private const val TOKEN_CACHE = 10 * 60 * 1000L
        private val networkThreads = Executors.newSingleThreadExecutor()
        @SuppressLint("StaticFieldLeak") // get() only stores the process-wide application context.
        @Volatile private var instance: PhoneRepository? = null
        fun get(context: Context): PhoneRepository = instance ?: synchronized(this) {
            instance ?: PhoneRepository(context.applicationContext).also { instance = it }
        }
    }
}
