#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
export JAVA_HOME="${JAVA_HOME:-$HOME/.local/jdk-17}"
export ANDROID_HOME="${ANDROID_HOME:-$HOME/.local/android-sdk}"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
if [ ! -x "$JAVA_HOME/bin/java" ]; then
    echo "JDK introuvable dans $JAVA_HOME" >&2
    exit 1
fi
if [ ! -d "$ANDROID_HOME/platforms/android-35" ]; then
    echo "Android SDK incomplet dans $ANDROID_HOME" >&2
    exit 1
fi
printf 'sdk.dir=%s\n' "$ANDROID_HOME" > "$root/android/local.properties"
cd "$root/android"
chmod +x ./gradlew
./gradlew --no-daemon test assembleRelease
mkdir -p "$root/dist"
cp -f "$root/android/app/build/outputs/apk/release/app-release.apk" "$root/dist/ai-usage.apk"
echo "APK: $root/dist/ai-usage.apk"
