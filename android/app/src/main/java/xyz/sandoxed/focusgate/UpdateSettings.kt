package xyz.sandoxed.focusgate

import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import kotlinx.coroutines.CancellationException
import java.io.File

private enum class UpdateAction { CHECK, DOWNLOAD }

@Composable
internal fun UpdateSettings(offline: Boolean) {
    val context = LocalContext.current
    val updater = remember(context.applicationContext) { AppUpdate(context.applicationContext) }
    val preview = BuildConfig.APPLICATION_ID != RELEASE_PACKAGE
    var action by remember { mutableStateOf<UpdateAction?>(null) }
    var status by remember { mutableStateOf<String?>(null) }
    var available by remember { mutableStateOf<AndroidUpdate?>(null) }
    var downloadedName by rememberSaveable { mutableStateOf<String?>(null) }
    val downloaded = downloadedName?.let(updater::cachedDownload)
    fun install(file: File) {
        if (!file.isFile) {
            downloadedName = null
            status = "The downloaded update expired. Download it again."
            return
        }
        try {
            if (Build.VERSION.SDK_INT >= 26 && !context.packageManager.canRequestPackageInstalls()) {
                context.startActivity(Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
                    Uri.parse("package:${context.packageName}")))
                status = "Allow installs for FocusGate, then return and tap Install update."
            } else {
                context.startActivity(updater.installIntent(file))
                status = "Confirm installation in Android’s installer."
            }
        } catch (_: ActivityNotFoundException) {
            status = "Android’s package installer is unavailable."
        } catch (_: SecurityException) {
            status = "Android blocked the install. Check your device’s installation settings."
        }
    }
    LaunchedEffect(action, offline) {
        val next = action ?: return@LaunchedEffect
        if (offline) {
            action = null
            status = null
            return@LaunchedEffect
        }
        try {
            when (next) {
                UpdateAction.CHECK -> {
                    status = "Checking GitHub Releases…"
                    val release = updater.check()
                    available = release?.update?.takeIf { it.versionCode > BuildConfig.VERSION_CODE }
                    status = when {
                        release == null -> "No release is published yet."
                        preview -> "Latest release ${release.tag}. Install the stable edition separately to receive in-app updates."
                        release.update == null -> "Release ${release.tag} has no Android update package."
                        available == null -> "FocusGate is up to date."
                        else -> "FocusGate ${release.tag} is available."
                    }
                }
                UpdateAction.DOWNLOAD -> {
                    status = "Downloading and verifying the update…"
                    val file = updater.download(checkNotNull(available))
                    downloadedName = file.name
                    install(file)
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            status = when (next) {
                UpdateAction.CHECK -> "Could not check GitHub Releases. Try again online."
                UpdateAction.DOWNLOAD -> "Could not download or verify the update."
            }
        }
        action = null
    }

    Text("App updates", style = MaterialTheme.typography.titleMedium)
    Text("Installed version ${BuildConfig.VERSION_NAME} (${BuildConfig.VERSION_CODE})")
    if (offline) Text("Go online to check for updates.")
    else status?.let { Text(it) }
    OutlinedButton(onClick = {
        available = null
        downloadedName = null
        action = UpdateAction.CHECK
    }, enabled = action == null && !offline) { Text("Check for updates") }
    if (preview) TextButton({
        openArticle(context, RELEASES_PAGE) { status = "No browser is available." }
    }, enabled = !offline) { Text("Open GitHub Releases") }
    if (available != null && downloaded == null) OutlinedButton({
        action = UpdateAction.DOWNLOAD
    }, enabled = action == null && !offline) { Text("Download and install") }
    downloaded?.let { file ->
        OutlinedButton(onClick = { install(file) }, enabled = action == null) { Text("Install update") }
    }
}
