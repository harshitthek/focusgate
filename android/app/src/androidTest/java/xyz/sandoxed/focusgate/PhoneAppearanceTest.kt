package xyz.sandoxed.focusgate

import android.content.ContextWrapper
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.assertEquals
import org.junit.Test
import org.junit.runner.RunWith
import java.util.UUID

@RunWith(AndroidJUnit4::class)
class PhoneAppearanceTest {
    @Test
    fun defaultsToOriginalAndRestoresEitherChoice() {
        val app = InstrumentationRegistry.getInstrumentation().targetContext
        val filename = "appearance-test-" + UUID.randomUUID()
        val testContext = object : ContextWrapper(app) {
            override fun getSharedPreferences(name: String, mode: Int) = super.getSharedPreferences(filename, mode)
        }

        try {
            assertEquals(PhoneAppearance.ORIGINAL, AppearancePreferences(testContext).read())
            AppearancePreferences(testContext).save(PhoneAppearance.MINIMALIST)
            assertEquals(PhoneAppearance.MINIMALIST, AppearancePreferences(testContext).read())
            AppearancePreferences(testContext).save(PhoneAppearance.ORIGINAL)
            assertEquals(PhoneAppearance.ORIGINAL, AppearancePreferences(testContext).read())
        } finally {
            app.deleteSharedPreferences(filename)
        }
    }
}
