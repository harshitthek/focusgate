package xyz.sandoxed.focusgate

import android.content.Context
import android.util.AtomicFile
import android.webkit.CookieManager
import kotlinx.coroutines.suspendCancellableCoroutine
import okhttp3.Call
import okhttp3.Callback
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.io.IOException
import java.security.MessageDigest
import java.text.SimpleDateFormat
import java.util.Locale
import java.util.concurrent.TimeUnit
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

private const val PAGE_SIZE = 50
private const val RESPONSE_LIMIT = 4L shl 20
private const val CACHE_LIMIT = 2L shl 20
private const val COOKIE = "focusgate_session"

/** Reuses the account authenticated in the dashboard and keeps a bounded private snapshot. */
internal class DashboardInbox(context: Context) {
    private val preferences = context.getSharedPreferences("dashboard", Context.MODE_PRIVATE)
    private val file = AtomicFile(File(context.noBackupFilesDir, "dashboard-inbox.json"))
    private val http = OkHttpClient.Builder().followRedirects(false).followSslRedirects(false)
        .callTimeout(10, TimeUnit.SECONDS).build()
    private var saved: Pair<String, List<Item>>? = null

    data class Access(val cookie: String, val key: String)

    fun belongsTo(profile: String): Boolean = preferences.getString("owner", profile) == profile

    fun claim(profile: String): Boolean {
        val alreadyOwned = preferences.contains("owner")
        return belongsTo(profile) && (alreadyOwned || preferences.edit().putString("owner", profile).commit())
    }

    fun forget() {
        preferences.edit().remove("owner").commit()
        saved = null
        file.delete()
    }

    fun access(profile: String): Access? {
        if (preferences.getString("owner", null) != profile) return null
        val value = CookieManager.getInstance().getCookie(BuildConfig.DASHBOARD_URL + "/api/items")
            ?.split(';')?.firstNotNullOfOrNull { it.trim().takeIf { part -> part.startsWith("$COOKIE=") }?.substringAfter('=') }
            ?.takeIf { it.matches(Regex("[A-Za-z0-9_-]{16,256}")) } ?: return null
        val identity = hash("${BuildConfig.DASHBOARD_URL}\u0000$profile\u0000$value")
        return Access("$COOKIE=$value", identity)
    }

    fun cached(access: Access?): List<Item> {
        if (access == null) return emptyList()
        saved?.let { if (it.first == access.key) return it.second }
        val cached = runCatching {
            file.baseFile.inputStream().use { input ->
                val bytes = ByteArray(CACHE_LIMIT.toInt() + 1)
                var count = 0
                while (count < bytes.size) {
                    val read = input.read(bytes, count, bytes.size - count)
                    if (read < 0) break
                    count += read
                }
                if (count == bytes.size) return emptyList()
                val data = JSONObject(String(bytes, 0, count, Charsets.UTF_8))
                if (data.getString("owner") != access.key) return emptyList()
                parseDashboardItems(data.getJSONArray("items"))
            }
        }.getOrElse { emptyList() }
        saved = access.key to cached
        return cached
    }

    suspend fun fetch(access: Access): JSONArray {
        val combined = JSONArray()
        for (offset in 0 until PAGE_SIZE * 2 step PAGE_SIZE) {
            val body = request(access, "/api/items?status=all&limit=$PAGE_SIZE&offset=$offset")
            val page = body.getJSONArray("items")
            if (page.length() > PAGE_SIZE) throw IOException("Dashboard returned too many items")
            for (index in 0 until page.length()) {
                val row = page.getJSONObject(index)
                combined.put(JSONObject()
                    .put("id", row.getLong("id"))
                    .put("source_type", row.optString("source_type").take(32))
                    .put("source_name", row.optString("source_name").take(128))
                    .put("sender", row.optString("sender").take(160))
                    .put("title", row.optString("title").take(512))
                    .put("body", row.optString("body").take(900))
                    .put("url", row.optString("url").take(2048))
                    .put("status", row.optString("status").take(24))
                    .put("priority", row.optInt("priority"))
                    .put("published_at", row.optString("published_at").take(64)))
            }
            if (page.length() < PAGE_SIZE) break
        }
        return combined
    }

    fun write(access: Access, items: JSONArray) {
        val parsed = parseDashboardItems(items)
        val bytes = JSONObject().put("owner", access.key).put("items", items).toString().toByteArray()
        if (bytes.size > CACHE_LIMIT) throw IOException("Dashboard inbox is too large")
        val output = file.startWrite()
        try {
            output.write(bytes)
            file.finishWrite(output)
        } catch (e: IOException) {
            file.failWrite(output)
            throw e
        }
        saved = access.key to parsed
    }

    fun invalidate(access: Access) {
        if (saved?.first == access.key) saved = null
        file.delete()
    }

    suspend fun mark(access: Access, id: Long, status: Int) {
        val action = if (status == 2) "archived" else "read"
        val body = JSONObject().put("status", action).toString().toRequestBody("application/json".toMediaType())
        request(access, "/api/items/$id/status", body)
    }

    private suspend fun request(access: Access, path: String, data: okhttp3.RequestBody? = null): JSONObject =
        suspendCancellableCoroutine { continuation ->
            val builder = Request.Builder().url(BuildConfig.DASHBOARD_URL + path)
                .header("Cookie", access.cookie).header("Accept", "application/json")
                .header("Cache-Control", "no-store")
            if (data != null) builder.post(data)
            val call = http.newCall(builder.build())
            continuation.invokeOnCancellation { call.cancel() }
            call.enqueue(object : Callback {
                override fun onFailure(call: Call, e: IOException) = continuation.resumeWithException(e)

                override fun onResponse(call: Call, response: Response) {
                    val result = runCatching {
                        response.use { res ->
                            if (res.code == 401 || res.code == 403) throw AuthError(res.code, "Sign in to the dashboard again")
                            if (!res.isSuccessful) throw IOException("Dashboard request failed (HTTP ${res.code})")
                            val source = res.body?.source() ?: throw IOException("Empty dashboard response")
                            if (source.request(RESPONSE_LIMIT + 1)) throw IOException("Dashboard response is too large")
                            JSONObject(source.readUtf8())
                        }
                    }
                    result.fold(continuation::resume, continuation::resumeWithException)
                }
            })
        }
}

internal fun dashboardProfile(issuer: String?, subject: String?): String =
    hash("${issuer.orEmpty()}\u0000${subject.orEmpty()}")

private fun hash(value: String): String =
    MessageDigest.getInstance("SHA-256").digest(value.toByteArray()).joinToString("") { "%02x".format(it) }

internal fun parseDashboardItems(rows: JSONArray): List<Item> {
    val date = SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss", Locale.ROOT).apply {
        timeZone = java.util.TimeZone.getTimeZone("UTC")
        isLenient = false
    }
    return List(rows.length()) { index ->
        val row = rows.getJSONObject(index)
        val id = row.getLong("id")
        require(id > 0)
        val source = row.getString("source_type")
        val name = row.getString("source_name")
        val type = if (source == "webhook" && name.equals("slack", ignoreCase = true)) "slack" else source
        val status = when (row.getString("status")) {
            "archived" -> 2
            "read" -> 1
            else -> 0
        }
        val published = runCatching { date.parse(row.getString("published_at"))?.time?.div(1000) ?: 0 }.getOrDefault(0)
        Item("dashboard:$id", row.getString("title"), row.getString("body"), row.getString("url"),
            row.getInt("priority").coerceIn(0, 5), status, published, "", type, name,
            dashboardId = id, digested = row.getString("status") == "digested", sender = row.optString("sender"))
    }.distinctBy { it.id }
}
