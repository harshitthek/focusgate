package xyz.sandoxed.focusgate

import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import okhttp3.HttpUrl.Companion.toHttpUrl
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File
import java.io.IOException

@RunWith(AndroidJUnit4::class)
class AppUpdateTest {
    private fun manifest() = JSONObject()
        .put("packageName", RELEASE_PACKAGE)
        .put("versionName", "1.2.3")
        .put("versionCode", 42)
        .put("size", 100)
        .put("sha256", "a".repeat(64))

    @Test
    fun acceptsOnlyMatchingReleaseMetadata() {
        assertEquals(AndroidUpdate("v1.2.3", 42, 100, "a".repeat(64)),
            parseUpdateManifest(manifest(), "v1.2.3", 100))
        for ((name, value) in mapOf(
            "packageName" to "$RELEASE_PACKAGE.preview",
            "versionName" to "1.2.4",
            "versionCode" to 0,
            "size" to 101,
            "sha256" to "",
        )) {
            assertThrows(IOException::class.java) {
                parseUpdateManifest(manifest().put(name, value), "v1.2.3", 100)
            }
        }
    }

    @Test
    fun restoresOnlyFocusGateDownloadsFromCache() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val updater = AppUpdate(context)
        assertEquals(File(context.cacheDir, "updates/focusgate-123.apk"), updater.cachedDownload("focusgate-123.apk"))
        for (name in listOf("../focusgate-123.apk", "focusgate-123.apk/other", "other.apk")) {
            assertNull(updater.cachedDownload(name))
        }
    }

    @Test
    fun limitsAssetRedirectsToGitHubOverHttps() {
        assertTrue(trustedReleaseRedirect("https://release-assets.githubusercontent.com/a".toHttpUrl()))
        for (url in listOf(
            "http://release-assets.githubusercontent.com/a",
            "https://release-assets.githubusercontent.com.example.test/a",
            "https://release-assets.githubusercontent.com:8443/a",
        )) {
            assertFalse(trustedReleaseRedirect(url.toHttpUrl()))
        }
    }
}
