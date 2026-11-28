package xyz.sandoxed.focusgate

import android.content.ClipData
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.content.FileProvider
import kotlinx.coroutines.suspendCancellableCoroutine
import okhttp3.Call
import okhttp3.Callback
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import org.json.JSONObject
import java.io.File
import java.io.IOException
import java.security.MessageDigest
import java.util.concurrent.TimeUnit
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

class UpdateFileProvider : FileProvider()

private const val REPO = "harshitthek/focusgate"
internal const val RELEASES_PAGE = "https://github.com/$REPO/releases"
internal const val RELEASE_PACKAGE = "xyz.sandoxed.focusgate"
private const val APK = "focusgate.apk"
private const val MANIFEST = "android-update.json"
private const val MAX_APK = 100L * 1024 * 1024
private const val MAX_RELEASE = 256L * 1024
private val stableTag = Regex("v(?:0|[1-9][0-9]*)\\.(?:0|[1-9][0-9]*)\\.(?:0|[1-9][0-9]*)")
private val sha256 = Regex("[0-9a-f]{64}")
private val cachedAPKName = Regex("focusgate-[A-Za-z0-9_-]+[.]apk")

internal data class AndroidRelease(val tag: String, val update: AndroidUpdate?)
internal data class AndroidUpdate(val tag: String, val versionCode: Long, val size: Long, val checksum: String)

internal class AppUpdate(private val context: Context) {
    private val http = OkHttpClient.Builder()
        .followRedirects(false).followSslRedirects(false)
        .connectTimeout(10, TimeUnit.SECONDS).readTimeout(20, TimeUnit.SECONDS)
        .callTimeout(3, TimeUnit.MINUTES).build()

    suspend fun check(): AndroidRelease? {
        val url = "https://api.github.com/repos/$REPO/releases/latest".toHttpUrl()
        val release = request(url, "application/vnd.github+json") { response ->
            if (response.code == 404) null else readJson(response, MAX_RELEASE)
        } ?: return null
        val tag = release.getString("tag_name")
        if (!stableTag.matches(tag) || tag.length > 40 ||
            release.optBoolean("draft") || release.optBoolean("prerelease")) throw IOException("Invalid release")

        if (context.packageName != RELEASE_PACKAGE) return AndroidRelease(tag, null)
        val assets = release.getJSONArray("assets")
        val sizes = (0 until assets.length()).map { assets.getJSONObject(it) }
            .filter { it.optString("state") == "uploaded" }.associate { it.getString("name") to it.getLong("size") }
        val size = sizes[APK]
        if (size == null || MANIFEST !in sizes) return AndroidRelease(tag, null)
        require(size in 1..MAX_APK)
        val update = asset(assetUrl(tag, MANIFEST)) {
            val manifest = readJson(it, 4096)
            parseUpdateManifest(manifest, tag, size)
        }
        return AndroidRelease(tag, update)
    }

    suspend fun download(update: AndroidUpdate): File =
        asset(assetUrl(update.tag, APK)) { response ->
            if (!response.isSuccessful) throw IOException("APK download returned HTTP ${response.code}")
            val directory = File(context.cacheDir, "updates")
            if (!directory.isDirectory && !directory.mkdirs()) throw IOException("Could not open update storage")
            val file = File.createTempFile("focusgate-", ".apk", directory)
            try {
                val body = response.body ?: throw IOException("Release APK is empty")
                val digest = MessageDigest.getInstance("SHA-256")
                var count = 0L
                body.byteStream().use { input ->
                    file.outputStream().use { output ->
                        val buffer = ByteArray(32 * 1024)
                        while (true) {
                            val read = input.read(buffer)
                            if (read < 0) break
                            count += read
                            if (count > update.size) throw IOException("APK size does not match the release")
                            digest.update(buffer, 0, read)
                            output.write(buffer, 0, read)
                        }
                    }
                }
                if (count != update.size || digest.digest().joinToString("") { "%02x".format(it) } != update.checksum) {
                    throw IOException("APK checksum or size does not match the release")
                }
                verifyPackage(file, update)
                directory.listFiles()?.filter { it != file && it.name.endsWith(".apk") }?.forEach { it.delete() }
                file
            } catch (e: Exception) {
                file.delete()
                throw e
            }
        }

    @Suppress("DEPRECATION")
    private fun verifyPackage(file: File, update: AndroidUpdate) {
        val pm = context.packageManager
        val flags = if (Build.VERSION.SDK_INT >= 28) PackageManager.GET_SIGNING_CERTIFICATES else PackageManager.GET_SIGNATURES
        val candidate = pm.getPackageArchiveInfo(file.absolutePath, flags) ?: throw IOException("Invalid APK")
        val installed = pm.getPackageInfo(context.packageName, flags)
        fun PackageInfo.code() = if (Build.VERSION.SDK_INT >= 28) longVersionCode else versionCode.toLong()
        fun PackageInfo.signers() = if (Build.VERSION.SDK_INT >= 28) signingInfo?.apkContentsSigners?.toSet() else signatures?.toSet()
        val currentSigners = installed.signers()
        if (candidate.packageName != installed.packageName || candidate.packageName != RELEASE_PACKAGE ||
            candidate.code() <= installed.code() || candidate.code() != update.versionCode ||
            candidate.versionName != update.tag.removePrefix("v") ||
            currentSigners.isNullOrEmpty() || candidate.signers() != currentSigners) {
            throw IOException("This APK cannot update the installed app")
        }
    }

    fun cachedDownload(name: String): File? = if (cachedAPKName.matches(name)) {
        File(File(context.cacheDir, "updates"), name)
    } else null

    fun installIntent(file: File): Intent {
        val uri = FileProvider.getUriForFile(context, "${context.packageName}.updates", file)
        return Intent(Intent.ACTION_VIEW).apply {
            setDataAndType(uri, "application/vnd.android.package-archive")
            clipData = ClipData.newRawUri("FocusGate update", uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
    }

    private fun assetUrl(tag: String, name: String) =
        "https://github.com/$REPO/releases/download/$tag/$name".toHttpUrl()

    private suspend fun <T> asset(start: HttpUrl, read: (Response) -> T): T {
        var url = start
        repeat(4) {
            val result = request(url, "application/octet-stream") { response ->
                if (response.code in setOf(301, 302, 303, 307, 308)) {
                    val next = response.header("Location")?.let(url::resolve)
                    if (next == null || !trustedReleaseRedirect(next)) throw IOException("Untrusted release download")
                    AssetResponse.Redirect(next)
                } else AssetResponse.Data(read(response))
            }
            when (result) {
                is AssetResponse.Redirect -> url = result.url
                is AssetResponse.Data -> return result.value
            }
        }
        throw IOException("Too many download redirects")
    }

    private sealed interface AssetResponse<out T> {
        data class Redirect(val url: HttpUrl) : AssetResponse<Nothing>
        data class Data<T>(val value: T) : AssetResponse<T>
    }

    private suspend fun <T> request(url: HttpUrl, accept: String, read: (Response) -> T): T =
        suspendCancellableCoroutine { continuation ->
            val request = Request.Builder().url(url).header("Accept", accept).build()
            val call = http.newCall(request)
            continuation.invokeOnCancellation { call.cancel() }
            call.enqueue(object : Callback {
                override fun onFailure(call: Call, e: IOException) = continuation.resumeWithException(e)
                override fun onResponse(call: Call, response: Response) {
                    runCatching { response.use(read) }.fold(continuation::resume, continuation::resumeWithException)
                }
            })
        }

    private fun readJson(response: Response, limit: Long): JSONObject {
        if (!response.isSuccessful) throw IOException("GitHub returned HTTP ${response.code}")
        val source = response.body?.source() ?: throw IOException("Empty release response")
        if (source.request(limit + 1)) throw IOException("Release response is too large")
        return JSONObject(source.readUtf8())
    }
}

internal fun parseUpdateManifest(json: JSONObject, tag: String, size: Long): AndroidUpdate {
    val code = json.getLong("versionCode")
    val checksum = json.getString("sha256")
    if (json.getString("packageName") != RELEASE_PACKAGE ||
        json.getString("versionName") != tag.removePrefix("v") ||
        json.getLong("size") != size || size !in 1..MAX_APK ||
        code !in 1..2_100_000_000L || !sha256.matches(checksum)) {
        throw IOException("Invalid Android update manifest")
    }
    return AndroidUpdate(tag, code, size, checksum)
}

internal fun trustedReleaseRedirect(url: HttpUrl): Boolean =
    url.isHttps && url.port == 443 && url.username.isEmpty() && url.password.isEmpty() &&
        url.host in setOf("release-assets.githubusercontent.com", "objects.githubusercontent.com")
