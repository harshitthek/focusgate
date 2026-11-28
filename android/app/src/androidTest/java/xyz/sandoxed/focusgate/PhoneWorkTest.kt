package xyz.sandoxed.focusgate

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import androidx.core.content.ContextCompat
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidx.work.Configuration
import androidx.work.NetworkType
import androidx.work.WorkInfo
import androidx.work.WorkManager
import androidx.work.testing.WorkManagerTestInitHelper
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assume.assumeTrue
import org.junit.Test
import org.junit.runner.RunWith
import java.util.concurrent.TimeUnit

@RunWith(AndroidJUnit4::class)
class PhoneWorkTest {
    private val context get() = InstrumentationRegistry.getInstrumentation().targetContext.applicationContext

    @Test
    fun independentSettingsUpdatesKeepNetworkPausedAndLocalDigestScheduled() {
        WorkManagerTestInitHelper.initializeTestWorkManager(context, Configuration.Builder().build())
        val repository = PhoneRepository.get(context)
        val before = repository.snapshot(false, 1)
        val profile = before.dashboardProfile
        val legacy = before.legacyView
        try {
            repository.updateSettings(profile, legacy) { it.copy(offline = false) }
            PhoneWork.schedule(context, online = true)
            val manager = WorkManager.getInstance(context)
            val feed = manager.getWorkInfosForUniqueWork(PhoneWork.FEEDS).get(10, TimeUnit.SECONDS).single()
            assertEquals(NetworkType.CONNECTED, feed.constraints.requiredNetworkType)
            assertEquals(30 * 60 * 1000L, feed.periodicityInfo?.repeatIntervalMillis)

            repository.updateSettings(profile, legacy) { it.copy(offline = true) }
            repository.updateSettings(profile, legacy) { it.copy(alerts = !before.settings.alerts) }
            val mode = if (before.settings.mode == "dnd") "normal" else "dnd"
            repository.updateSettings(profile, legacy) { it.copy(mode = mode) }

            val after = repository.snapshot(false, 1)
            assertEquals(PhoneSettings(true, !before.settings.alerts, mode), after.settings)
            val cancelled = requireNotNull(manager.getWorkInfoById(feed.id).get(10, TimeUnit.SECONDS))
            assertEquals(WorkInfo.State.CANCELLED, cancelled.state)
            val digest = manager.getWorkInfosForUniqueWork(PhoneWork.DIGEST).get(10, TimeUnit.SECONDS).single()
            assertEquals(NetworkType.NOT_REQUIRED, digest.constraints.requiredNetworkType)
            assertNotNull(digest.periodicityInfo)
        } finally {
            repository.updateSettings(profile, legacy) { before.settings }
        }
    }

    @Test
    fun deniedNotificationPermissionLeavesTheLocalInboxReadable() {
        assumeTrue(Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED)
        val repository = PhoneRepository.get(context)
        repository.deliver()
        assertNotNull(repository.snapshot(false, 1).settings)
    }
}
