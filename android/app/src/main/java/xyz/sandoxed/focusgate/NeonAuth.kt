package xyz.sandoxed.focusgate

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import kotlinx.coroutines.suspendCancellableCoroutine
import okhttp3.Call
import okhttp3.Callback
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.json.JSONObject
import java.io.IOException
import java.net.URI
import java.security.KeyStore
import java.util.concurrent.TimeUnit
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

internal data class Account(val issuer: String, val subject: String, val email: String, val cookie: String)
internal data class Login(val account: Account, val token: String)

internal class SessionVault(context: Context) {
    private val prefs = context.getSharedPreferences("session", Context.MODE_PRIVATE)
    private val alias = "focusgate.neon.session"

    private fun key(create: Boolean): SecretKey? {
        val store = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        val existing = store.getKey(alias, null) as? SecretKey
        if (existing != null || !create) return existing
        return KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore").run {
            init(KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE).build())
            generateKey()
        }
    }

    fun load(): Account? {
        val record = prefs.getString("account", null) ?: return null
        val encoded = record.split('.', limit = 2)
        if (encoded.size != 2) return null
        return runCatching {
            val decryptionKey = key(false) ?: return null
            val cipher = Cipher.getInstance("AES/GCM/NoPadding")
            cipher.init(Cipher.DECRYPT_MODE, decryptionKey, GCMParameterSpec(128, Base64.decode(encoded[0], Base64.NO_WRAP)))
            val data = JSONObject(String(cipher.doFinal(Base64.decode(encoded[1], Base64.NO_WRAP)), Charsets.UTF_8))
            Account(data.getString("issuer"), data.getString("subject"), data.getString("email"), data.getString("cookie"))
        }.getOrNull()
    }

    fun save(account: Account) {
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.ENCRYPT_MODE, key(true))
        val plaintext = JSONObject().put("issuer", account.issuer).put("subject", account.subject)
            .put("email", account.email).put("cookie", account.cookie).toString()
        val encrypted = Base64.encodeToString(cipher.iv, Base64.NO_WRAP) + "." +
            Base64.encodeToString(cipher.doFinal(plaintext.toByteArray(Charsets.UTF_8)), Base64.NO_WRAP)
        check(prefs.edit().putString("account", encrypted).commit()) { "Could not save the session" }
    }

    fun clear() {
        if (!prefs.edit().remove("account").commit()) {
            KeyStore.getInstance("AndroidKeyStore").apply { load(null); deleteEntry(alias) }
        }
    }
}

internal class AuthError(val status: Int, message: String) : IOException(message)

internal class NeonAuth(
    private val base: String = BuildConfig.NEON_AUTH_URL,
    private val sync: String = BuildConfig.SYNC_URL,
) {
    val configured get() = base.isNotBlank() && sync.isNotBlank()
    private val origin get() = URI(base).let { "${it.scheme}://${it.rawAuthority}" }
    private val json = "application/json".toMediaType()
    private val http = OkHttpClient.Builder().followRedirects(false).followSslRedirects(false)
        .callTimeout(10, TimeUnit.SECONDS).build()

    suspend fun register(name: String, email: String, password: String) {
        requestAuth("/sign-up/email", JSONObject().put("name", name).put("email", email).put("password", password))
    }

    suspend fun sendVerification(email: String) {
        requestAuth("/email-otp/send-verification-otp",
            JSONObject().put("email", email).put("type", "email-verification"))
    }

    suspend fun verify(email: String, code: String) {
        requestAuth("/email-otp/verify-email", JSONObject().put("email", email).put("otp", code))
    }

    suspend fun signIn(email: String, password: String): Login {
        val signedIn = requestAuth("/sign-in/email", JSONObject().put("email", email).put("password", password))
        val cookie = signedIn.cookie ?: throw AuthError(401, "Neon did not return a session")
        val auth = session(cookie)
        val identity = request(Request.Builder().url("$sync/v1/identity").header("Authorization", "Bearer ${auth.token}")
            .post("{}".toRequestBody(json)).build()).body
        if (identity.getString("issuer") != origin || identity.getString("subject") != auth.user.getString("id")) {
            throw AuthError(401, "Neon account identity could not be verified")
        }
        return Login(Account(origin, identity.getString("subject"), auth.user.getString("email"), auth.cookie), auth.token)
    }

    suspend fun refresh(account: Account): Login {
        if (account.issuer != origin) throw AuthError(401, "Sign in again to sync")
        val auth = session(account.cookie)
        if (auth.user.getString("id") != account.subject) throw AuthError(401, "Session account changed")
        return Login(account.copy(cookie = auth.cookie), auth.token)
    }

    suspend fun end(cookie: String) {
        requestAuth("/sign-out", JSONObject(), cookie)
    }

    private data class Session(val cookie: String, val user: JSONObject, val token: String)
    private data class Payload(val body: JSONObject, val cookie: String?, val jwt: String?)

    private suspend fun session(cookie: String): Session {
        val checked = requestAuth("/get-session?disableCookieCache=true", cookie = cookie)
        val user = checked.body.optJSONObject("user") ?: throw AuthError(401, "Sign in again to sync")
        if (!user.optBoolean("emailVerified")) throw AuthError(403, "Verify your email to sync")
        val rotated = checked.cookie ?: cookie
        val fallback = if (checked.jwt == null) requestAuth("/token", cookie = rotated) else null
        val token = checked.jwt ?: fallback?.body?.optString("token").orEmpty()
        if (token.isBlank() || token.length > 8192) throw AuthError(401, "Neon access token unavailable")
        return Session(fallback?.cookie ?: rotated, user, token)
    }

    private suspend fun requestAuth(path: String, data: JSONObject? = null, cookie: String? = null): Payload {
        check(configured) { "Cloud sync is unavailable in this build" }
        val req = Request.Builder().url("$base$path").header("Origin", origin)
        cookie?.let { req.header("Cookie", it) }
        if (data != null) req.post(data.toString().toRequestBody(json))
        return request(req.build())
    }

    private suspend fun request(request: Request): Payload = suspendCancellableCoroutine { continuation ->
        val call = http.newCall(request)
        continuation.invokeOnCancellation { call.cancel() }
        call.enqueue(object : Callback {
            override fun onFailure(call: Call, e: IOException) {
                continuation.resumeWithException(e)
            }

            override fun onResponse(call: Call, response: Response) {
                val value = runCatching {
                    response.use {
                        if (!it.isSuccessful) {
                            val message = when (it.code) {
                                401 -> "Sign-in failed or the session expired"
                                403 -> "Verify your email or check the Neon Auth configuration"
                                429 -> "Too many attempts. Try again later."
                                else -> "Neon returned HTTP ${it.code}"
                            }
                            throw AuthError(it.code, message)
                        }
                        val source = it.body?.source()
                        if (source?.request(65_537L) == true) throw IOException("Neon response is too large")
                        val raw = source?.readUtf8().orEmpty()
                        val body = if (raw.isBlank() || raw == "null") JSONObject() else JSONObject(raw)
                        val cookie = it.headers("Set-Cookie").asSequence().map { header -> header.substringBefore(';') }
                            .lastOrNull { pair -> pair.substringBefore('=').endsWith(".session_token") &&
                                pair.substringAfter('=', "").isNotBlank() && pair.length <= 4096 }
                        Payload(body, cookie, it.header("set-auth-jwt"))
                    }
                }
                value.fold(continuation::resume, continuation::resumeWithException)
            }
        })
    }
}
