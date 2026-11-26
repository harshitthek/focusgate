package xyz.sandoxed.focusgate

import android.content.Context
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.withContext
import java.util.concurrent.TimeUnit

class PhoneWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {
    override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
        var phone: PhoneRepository? = null
        try {
            val repository = PhoneRepository.get(applicationContext)
            phone = repository
            when (inputData.getString("action")) {
                "digest" -> repository.deliver()
                "sync" -> repository.refresh(fetchFeeds = false)
                else -> repository.refresh(fetchFeeds = true)
            }
            Result.success()
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            if (runAttemptCount < 2) Result.retry() else Result.failure()
        } finally {
            phone?.updates?.update { it + 1 }
        }
    }
}

internal object PhoneWork {
    const val FEEDS = "focusgate.feed.refresh"
    const val DIGEST = "focusgate.digest"
    private const val SYNC = "focusgate.sync"
    private val onlineConstraint = Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build()

    fun schedule(context: Context, online: Boolean) {
        val work = WorkManager.getInstance(context)
        if (online) {
            work.enqueueUniquePeriodicWork(FEEDS, ExistingPeriodicWorkPolicy.KEEP,
                PeriodicWorkRequestBuilder<PhoneWorker>(30, TimeUnit.MINUTES).setConstraints(onlineConstraint).build())
        }
        work.enqueueUniquePeriodicWork(DIGEST, ExistingPeriodicWorkPolicy.KEEP,
            PeriodicWorkRequestBuilder<PhoneWorker>(15, TimeUnit.MINUTES)
                .setInputData(workDataOf("action" to "digest")).build())
    }

    fun pauseNetwork(context: Context) {
        val work = WorkManager.getInstance(context)
        work.cancelUniqueWork(FEEDS)
        work.cancelUniqueWork(SYNC)
    }

    fun syncSoon(context: Context) {
        WorkManager.getInstance(context).enqueueUniqueWork(SYNC, ExistingWorkPolicy.KEEP,
            OneTimeWorkRequestBuilder<PhoneWorker>().setConstraints(onlineConstraint)
                .setInputData(workDataOf("action" to "sync")).build())
    }
}
