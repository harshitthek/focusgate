plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

fun endpoint(name: String): String {
    val raw = providers.environmentVariable(name).orNull.orEmpty().trim().trimEnd('/')
    require(raw.isEmpty() || Regex("https://[A-Za-z0-9._:/-]+").matches(raw)) { "Invalid HTTPS URL in $name" }
    return "\"$raw\""
}

android {
    namespace = "xyz.sandoxed.focusgate"
    compileSdk = 36

    defaultConfig {
        applicationId = "xyz.sandoxed.focusgate"
        minSdk = 24
        targetSdk = 36
        versionCode = providers.environmentVariable("FOCUSGATE_APP_BUILD").orNull?.toInt() ?: 1
        versionName = providers.environmentVariable("FOCUSGATE_APP_VERSION").orNull ?: "1.0.0"
        buildConfigField("String", "NEON_AUTH_URL", endpoint("FOCUSGATE_NEON_AUTH_URL"))
        buildConfigField("String", "SYNC_URL", endpoint("FOCUSGATE_SYNC_URL"))
        buildConfigField("String", "ASSESSMENT_URL", endpoint("FOCUSGATE_ASSESSMENT_URL"))
        buildConfigField("String", "DASHBOARD_URL", endpoint("FOCUSGATE_DASHBOARD_URL").takeUnless { it == "\"\"" } ?: "\"https://sandoxed.xyz\"")
        ndk.abiFilters += listOf("arm64-v8a", "x86_64")
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    buildTypes {
        debug { applicationIdSuffix = ".preview" }
    }

    buildFeatures { compose = true; buildConfig = true }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

kotlin { compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) } }

dependencies {
    implementation(files("libs/phone.aar"))
    implementation("androidx.core:core-ktx:1.16.0")
    implementation("androidx.activity:activity-compose:1.10.1")
    implementation(platform("androidx.compose:compose-bom:2025.06.01"))
    implementation("androidx.compose.material3:material3")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.9.1")
    implementation("androidx.work:work-runtime-ktx:2.10.2")
    implementation("com.squareup.okhttp3:okhttp:4.12.0")

    androidTestImplementation("androidx.test:runner:1.6.2")
    androidTestImplementation("androidx.test.ext:junit:1.2.1")
    androidTestImplementation("androidx.work:work-testing:2.10.2")
}
