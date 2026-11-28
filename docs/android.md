# FocusGate for Android

The Android app stores subscriptions, inbox, focus mode, and alert settings in private SQLite databases. It fetches public RSS and Atom feeds from the phone. The local profile works without an account. Each verified Neon account gets its own profile on each phone.

FocusGate collects GitHub notifications, email, Slack mentions, webhooks, and feeds in one inbox. **Original** opens the same FocusGate dashboard served at [sandoxed.xyz](https://sandoxed.xyz), with its source controls, queue, and Retro Pager. Choose **Minimalist** under **Settings → Appearance** for the native inbox. The phone remembers the choice across app restarts and account switches.

Sign in to the dashboard with your existing FocusGate web account to see server notifications. Use **Saved inbox** to fetch up to the 100 newest server items onto the phone. The native inbox shows those items alongside feeds fetched by the phone. It can search and read saved previews offline. Updating a server item’s read or archive state requires a connection. Dashboard sessions and saved previews belong to the local or Neon profile that opened the dashboard. Neon sign-in separately syncs the phone's RSS subscriptions and item state.

The APK uses sandoxed.xyz as its default dashboard. For a self-hosted build, set `FOCUSGATE_DASHBOARD_URL` to the dashboard's HTTPS origin before building.

## Update the Android app

In **Settings → App updates**, choose **Check for updates**. For a newer stable release, tap **Download and install** and confirm in Android's installer. On first use, Android may ask you to allow FocusGate to install apps. Return to FocusGate after granting permission and tap **Install update**.

The release workflow publishes `focusgate.apk` and `android-update.json` for stable `vMAJOR.MINOR.PATCH` tags. FocusGate checks the APK's size, checksum, package, version, and signing certificate before offering installation. PR and staging APKs use the separate `xyz.sandoxed.focusgate.preview` package. Their update checker shows the latest stable release and links to GitHub. Install the stable edition separately to use in-app updates.

Offline mode pauses dashboard requests, direct feed requests, account requests, sync, and remote priority assessment. You can still read saved notifications, archive items fetched by the phone, edit subscriptions, and change the phone's focus mode. The next online refresh fetches new entries and syncs pending changes. With a feed's permission, a signed-in phone can send an ambiguous item's title and up to 900 characters of text to the optional assessment service.

## Connect Slack

Create a Slack app for the workspace, enable Events API subscriptions, and set its request URL to `https://YOUR-FOCUSGATE-HOST/api/slack/events`. Set `sources.slack.enabled` to `true` and provide `FOCUSGATE_SLACK_SIGNING_SECRET` and `FOCUSGATE_SLACK_WORKSPACE_ID` in the server environment. Subscribe to `app_mention` with `app_mentions:read` and invite the app to the channels it should monitor.

To receive personal mentions, set `FOCUSGATE_SLACK_MENTION_USER_ID` to your Slack member ID and subscribe to `message.channels` with `channels:history` or `message.groups` with `groups:history` for private channels where the app is a member. FocusGate saves messages that mention the configured user and ignores other channel messages. Slack events go to the server operator's inbox. [Slack Events API](https://docs.slack.dev/apis/events-api/)

Background feed refresh is scheduled every 30 minutes when connected. Android and device battery settings control the actual timing. Local digests become due at 09:00, 13:30, and 19:30 in the device's time zone. New installations, first feed backlogs, migrated history, and cloud restores open silently. To inspect the retained history from a previous Fyne installation, open **Previous inbox** under Settings. This archive stays on the phone and is excluded from the Neon import.

## Build and install

The preview package is `xyz.sandoxed.focusgate.preview`. The signed release package is `xyz.sandoxed.focusgate`. The preview and release include ARM64 and x86-64. Use Java 17 or 21 and install Android SDK Platform 36, Build Tools 35.0.0, and NDK 28.2.13676358.

Build the AAR through the command below. It uses Android-compatible file syscalls for the x86-64 SQLite library in the APK and leaves the pure-Go daemon build independent.

On Linux, run `bash scripts/build-android.sh preview`. On Windows PowerShell:

```powershell
$env:ANDROID_NDK_HOME = Join-Path $env:ANDROID_HOME 'ndk/28.2.13676358'
$env:Path = "$(go env GOPATH)\bin;$env:Path"
go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind
go run ./scripts/android-bind
android/gradlew.bat -p android :app:lintDebug :app:assembleDebug :app:assembleDebugAndroidTest
& "$env:ANDROID_HOME/platform-tools/adb.exe" install -r android/app/build/outputs/apk/debug/app-debug.apk
```

To exercise the instrumented tests with a connected device or emulator, run `android/gradlew.bat -p android :app:connectedDebugAndroidTest` on Windows or `bash android/gradlew -p android :app:connectedDebugAndroidTest` on Linux.

## Set up a staging Neon branch

1. Use an AWS project in a region with Neon Functions. Start with a staging branch. Enable Managed Neon Auth and email verification by code. Add the Auth service's HTTPS origin to the branch's trusted domains if the branch configuration requires it.
2. In `services/sync`, run `npm ci`. Apply [the sync schema](../services/sync/schema.sql) to the staging database with `psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f schema.sql`. Use the branch connection string in the shell environment. The function receives the branch database URL, Auth URL and JWKS URL from Neon.
3. From the same directory, deploy the function to the staging branch with the Neon CLI using the included `neon.ts`. Record the Auth base URL and the function invocation URL. Verify the branch's `/open-api/generate-schema` includes the email OTP, session, and token endpoints before connecting a client.
4. Set `FOCUSGATE_NEON_AUTH_URL` to the branch's Auth base URL and `FOCUSGATE_SYNC_URL` to the function invocation URL before building the APK. Set `FOCUSGATE_ASSESSMENT_URL` only if staging assessment is enabled. These URLs are public app configuration. Put the production URLs in the equivalent GitHub Actions repository variables for tagged builds.

Phone sync posts at most 50 pending operations per request. Postgres scopes rows to the JWT issuer and subject, serializes changes per account, and returns up to 50 changes. The archive state is monotone. If two devices edit a feed from the same revision, the losing device can review and reapply its edit. Synced history is limited to 30 days and 1,000 entries per account. Focus mode, feed assessment opt-in, notification receipts, and fetch validators remain device settings. Provision a staging email account and test signup, verification, sign-in, cookie refresh, and logout before configuring the release URLs.

## Optional assessment on vm1

Set `FOCUSGATE_ASSESSMENT_PROVIDER=jev`, `FOCUSGATE_NEON_AUTH_URL`, and `FOCUSGATE_ASSESSMENT_JEV_API_KEY` in the daemon environment. The dedicated key uses `https://api.typesafe.ai` by default. Set `FOCUSGATE_ASSESSMENT_JEV_BASE_URL` if that key belongs to a different Jev HTTPS endpoint. Without a dedicated key, assessment uses the configured daemon classifier credentials and endpoint. Use the same Neon Auth branch as the app and expose the daemon through HTTPS. Enable assessment for a feed on the phone only after the user accepts sending item excerpts.

To qualify a replacement model, run it as a separate service bound to a loopback IP with one worker and its own memory cap. Set `FOCUSGATE_ASSESSMENT_PROVIDER=local-model` and `FOCUSGATE_LOCAL_MODEL_URL=http://127.0.0.1:port/path`. The model accepts JSON with `title` and `snippet`. It returns `model`, `actionable_prob`, `time_sensitive_prob`, `category`, `category_prob`, `category_confidence`, `urgency_score` (0–3), and `urgency_confidence`. Each probability is between 0 and 1. Categories are `security_or_outage`, `direct_person_or_review`, `work_or_dev_update`, `news_or_reading`, or `promo_or_noise`. The daemon limits assessment to one concurrent request and three seconds. Models produce signals. Phone rules and current focus mode decide delivery.

Benchmark memory and swap with the FocusGate daemon running before enabling the model on vm1. Compare urgent-alert recall and false DND breakthroughs against Jev using the same examples. Revert to `jev` or set `FOCUSGATE_ASSESSMENT_PROVIDER=disabled` if the model fails qualification. Feed refresh, local ranking, and Neon sync work independently of that endpoint.

## Release checks

- Install the signed APK over the previous stable APK on the Redmi. Confirm identical signing identity, a higher version code, preserved data, and access to the previous inbox when applicable.
- Fetch a feed with vm1 unavailable. Enter airplane mode, archive an entry, restart the app, then reconnect. Sign in on another phone and confirm that the archive state and subscriptions restore silently.
- Exercise explicit Offline mode during feed and sync requests, enabling alerts with the Android permission denied and granted, changing focus during an assessment, screen-off refresh, reboot, and digest delivery. A fresh urgent item should alert once under the current phone policy. A provider error should leave the local ranking and delivery behavior in effect.
