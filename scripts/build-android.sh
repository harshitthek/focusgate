#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
variant="${1:?usage: build-android.sh preview|release}"

: "${ANDROID_HOME:?Android SDK location is required}"
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/28.2.13676358"
export PATH="$(go env GOPATH)/bin:$PATH"
export FOCUSGATE_APP_BUILD="${FOCUSGATE_APP_BUILD:-${GITHUB_RUN_NUMBER:-1}}"

go install golang.org/x/mobile/cmd/gomobile golang.org/x/mobile/cmd/gobind
go run ./scripts/android-bind

build_tools="$ANDROID_HOME/build-tools/35.0.0"
case "$variant" in
  preview)
    bash android/gradlew -p android --no-daemon :app:lintDebug :app:assembleDebug :app:assembleDebugAndroidTest
    apk=focusgate-preview.apk
    package=xyz.sandoxed.focusgate.preview
    cp android/app/build/outputs/apk/debug/app-debug.apk "$apk"
    ;;
  release)
    : "${FOCUSGATE_ANDROID_KEYSTORE:?Persistent release keystore is required}"
    : "${FOCUSGATE_NEON_AUTH_URL:?Neon Auth URL is required for a dual-mode release}"
    : "${FOCUSGATE_SYNC_URL:?Neon sync URL is required for a dual-mode release}"
    : "${FOCUSGATE_APP_VERSION:?Set the Android release version}"
    [[ $FOCUSGATE_APP_VERSION =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
    [[ ${#FOCUSGATE_APP_VERSION} -lt 40 ]]
    [[ $FOCUSGATE_APP_BUILD =~ ^[1-9][0-9]{0,9}$ && $FOCUSGATE_APP_BUILD -le 2100000000 ]]
    bash android/gradlew -p android --no-daemon :app:assembleRelease
    mkdir -p artifacts
    apk=artifacts/focusgate.apk
    package=xyz.sandoxed.focusgate
    "$build_tools/apksigner" sign --ks "$FOCUSGATE_ANDROID_KEYSTORE" --ks-pass env:ANDROID_KEYSTORE_PASSWORD \
      --ks-key-alias "$ANDROID_KEY_ALIAS" --key-pass env:ANDROID_KEY_PASSWORD --out "$apk" \
      android/app/build/outputs/apk/release/app-release-unsigned.apk
    ;;
  *) exit 2 ;;
esac

"$build_tools/apksigner" verify --verbose "$apk"
badging=$("$build_tools/aapt" dump badging "$apk")
[[ $badging == *"package: name='$package' "* ]]
[[ $badging == *"sdkVersion:'24'"* ]]
[[ $badging == *"targetSdkVersion:'36'"* ]]
[[ $badging == *"uses-permission: name='android.permission.POST_NOTIFICATIONS'"* ]]
[[ $badging != *"android.permission.WRITE_EXTERNAL_STORAGE"* ]]
[[ $badging == *"arm64-v8a"* && $badging == *"x86_64"* ]]
"$build_tools/zipalign" -c -P 16 4 "$apk"

if [[ $variant == release ]]; then
  [[ $badging == *"versionCode='$FOCUSGATE_APP_BUILD' versionName='$FOCUSGATE_APP_VERSION'"* ]]
  read -r checksum _ < <(sha256sum -- "$apk")
  size=$(wc -c < "$apk")
  [[ $size -gt 0 && $size -le 104857600 ]]
  printf '{"packageName":"%s","versionName":"%s","versionCode":%s,"size":%s,"sha256":"%s"}\n' \
    "$package" "$FOCUSGATE_APP_VERSION" "$FOCUSGATE_APP_BUILD" "$size" "$checksum" > artifacts/android-update.json
fi
