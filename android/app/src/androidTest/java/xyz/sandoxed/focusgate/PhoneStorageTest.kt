package xyz.sandoxed.focusgate

import androidbridge.Androidbridge
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File
import java.util.UUID

@RunWith(AndroidJUnit4::class)
class PhoneStorageTest {
    @Test
    fun offlineChangesPersistAndProfilesStayIsolated() {
        val context = InstrumentationRegistry.getInstrumentation().targetContext
        val dir = File(context.cacheDir, UUID.randomUUID().toString())
        val local = Androidbridge.open(dir.absolutePath, "", "")
        try {
            local.settings("""{"offline":true,"alerts":false,"mode":"dnd"}""")
            local.saveFeed("""{"name":"Example","url":"https://example.org/feed"}""")
            assertTrue(runCatching { local.beginNetwork("offline") }.isFailure)
        } finally {
            local.close()
        }
        val reopened = Androidbridge.open(dir.absolutePath, "", "")
        val account = Androidbridge.open(dir.absolutePath, "https://auth.example.org", "account1")
        try {
            val localState = JSONObject(reopened.state(false, 0))
            assertTrue(localState.getJSONObject("settings").getBoolean("offline"))
            assertEquals("dnd",localState.getJSONObject("settings").getString("mode"))
            assertEquals(1,localState.getJSONArray("feeds").length())
            val accountState = JSONObject(account.state(false, 0))
            assertFalse(accountState.getJSONObject("settings").getBoolean("offline"))
            assertEquals(0,accountState.getJSONArray("feeds").length())
            assertEquals("[]",account.notices("UTC",0))
        } finally {
            reopened.close()
            account.close()
            dir.deleteRecursively()
        }
    }
}
