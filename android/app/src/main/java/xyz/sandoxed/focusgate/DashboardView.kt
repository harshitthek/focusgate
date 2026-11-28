package xyz.sandoxed.focusgate

import android.annotation.SuppressLint
import android.net.Uri
import android.webkit.CookieManager
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebResourceResponse
import android.webkit.WebSettings
import android.webkit.WebChromeClient
import android.webkit.WebStorage
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import java.io.ByteArrayInputStream

@SuppressLint("SetJavaScriptEnabled")
@Composable
internal fun DashboardView(
    vm: PhoneViewModel,
    state: PhoneState,
    showInbox: () -> Unit,
) {
    var ready by remember { mutableStateOf<Boolean?>(null) }
    var loadFailed by remember { mutableStateOf(false) }
    var attempt by remember { mutableIntStateOf(0) }
    LaunchedEffect(state.dashboardProfile, state.dashboardAllowed, attempt) {
        ready = vm.claimDashboard()
    }
    if (ready != true || loadFailed || !state.dashboardAllowed) {
        Column(Modifier.fillMaxSize().padding(20.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Text(when {
                loadFailed -> "The dashboard is unavailable."
                !state.dashboardAllowed -> "This dashboard session belongs to another profile on this phone."
                ready == false -> "The dashboard could not open."
                else -> "Opening FocusGate…"
            })
            if (loadFailed) Button({
                loadFailed = false
                attempt++
            }) { Text("Retry") }
            if (!state.dashboardAllowed) TextButton({
                // This is the app's sole WebView origin. Drop its existing session before exposing another profile.
                CookieManager.getInstance().removeAllCookies {
                    CookieManager.getInstance().flush()
                    WebStorage.getInstance().deleteAllData()
                    vm.forgetDashboard()
                }
            }) { Text("Sign in as a different dashboard user") }
            TextButton(showInbox) { Text("Open saved inbox") }
        }
        return
    }
    val origin = Uri.parse(BuildConfig.DASHBOARD_URL)
    key(attempt) {
        AndroidView(
            modifier = Modifier.fillMaxSize(),
            factory = { context ->
                WebView(context).apply {
                    settings.javaScriptEnabled = true
                    settings.domStorageEnabled = true
                    settings.allowFileAccess = false
                    settings.allowContentAccess = false
                    settings.mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
                    settings.setSupportMultipleWindows(false)
                    webChromeClient = WebChromeClient()
                    CookieManager.getInstance().setAcceptThirdPartyCookies(this, false)
                    webViewClient = object : WebViewClient() {
                        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                            if (sameDashboardOrigin(request.url, origin)) return false
                            if (request.isForMainFrame) {
                                openArticle(context, request.url.toString()) { vm.error = "No browser is available" }
                            }
                            return true
                        }

                        override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
                            if (sameDashboardOrigin(request.url, origin)) return null
                            return WebResourceResponse("text/plain", "UTF-8", 403, "Forbidden",
                                emptyMap(), ByteArrayInputStream(byteArrayOf()))
                        }

                        override fun onPageStarted(view: WebView, url: String, favicon: android.graphics.Bitmap?) {
                            if (!sameDashboardOrigin(Uri.parse(url), origin)) {
                                view.stopLoading()
                                loadFailed = true
                            }
                        }

                        override fun onReceivedError(view: WebView, request: WebResourceRequest, error: WebResourceError) {
                            if (request.isForMainFrame) loadFailed = true
                        }

                        override fun onReceivedHttpError(view: WebView, request: WebResourceRequest, errorResponse: WebResourceResponse) {
                            if (request.isForMainFrame && errorResponse.statusCode >= 400) loadFailed = true
                        }
                    }
                    loadUrl(BuildConfig.DASHBOARD_URL + "/")
                }
            },
            onRelease = { view ->
                view.stopLoading()
                view.settings.blockNetworkLoads = true
                CookieManager.getInstance().flush()
                view.destroy()
            },
        )
    }
}

internal fun sameDashboardOrigin(url: Uri, origin: Uri): Boolean =
    origin.scheme == "https" && !origin.host.isNullOrBlank() &&
        url.scheme == "https" && url.userInfo == null && url.host.equals(origin.host, ignoreCase = true) &&
        (if (url.port == -1) 443 else url.port) == (if (origin.port == -1) 443 else origin.port)
