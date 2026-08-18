#!/usr/bin/env bash
# Package the SwiftUI menubar app as a macOS bundle.
# No Xcode project: swift build -c release, then assemble the bundle by
# hand. Release builds are universal (arm64 + x86_64) so one artifact supports
# Apple Silicon and Intel. Builds on either host architecture.
#
# Usage:
#   scripts/build-menubar.sh                    # Stable, the default
#   scripts/build-menubar.sh --variant stable
#   scripts/build-menubar.sh --variant preview
#   scripts/build-menubar.sh --variant stable --release
#
# Local builds use an ad hoc development signature. --release requires
# the explicit ad-hoc beta release mode, a numeric marketing version,
# and a numeric build number.
#
# Outputs:
#   stable:  mac/BasetenSwitch/dist/Baseten Switch.app
#   preview: mac/BasetenSwitch/dist-preview/Baseten Switch Preview.app
#
# Stable remains the default to preserve the existing local and release
# build contract. Preview is selected only by an explicit argument, never
# by ambient environment, so release automation cannot accidentally emit
# the Preview identity.

set -euo pipefail
cd "$(dirname "$0")/../mac/BasetenSwitch"

fail() {
    echo "error: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Usage: scripts/build-menubar.sh [--variant stable|preview] [--release]

Build the universal macOS menubar app. Stable is the default.

Development builds use an ad hoc signature. Beta release builds require:
  BASETEN_SWITCH_RELEASE_SIGNING_MODE  Must be "adhoc"
  BASETEN_SWITCH_MARKETING_VERSION     Numeric version, for example 0.2.0
  BASETEN_SWITCH_BUILD_NUMBER          Numeric build, for example 42
EOF
}

VARIANT="stable"
RELEASE_BUILD=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --variant)
            [[ $# -ge 2 && -n "$2" && "$2" != -* ]] \
                || fail "--variant requires stable or preview"
            VARIANT="$2"
            shift 2
            ;;
        --release)
            RELEASE_BUILD=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            fail "unknown argument: $1 (usage: $0 [--variant stable|preview] [--release])"
            ;;
    esac
done

case "$VARIANT" in
    stable)
        BUNDLE_ID="co.baseten.switch"
        APP_NAME="Baseten Switch"
        EXECUTABLE_NAME="BasetenSwitch"
        BUILD_CHANNEL="stable"
        DIST_DIR="dist"
        ;;
    preview)
        BUNDLE_ID="co.baseten.switch.preview"
        APP_NAME="Baseten Switch Preview"
        EXECUTABLE_NAME="BasetenSwitchPreview"
        BUILD_CHANNEL="preview"
        DIST_DIR="dist-preview"
        ;;
    *)
        fail "unsupported variant '$VARIANT' (expected stable or preview)"
        ;;
esac

if [[ "$RELEASE_BUILD" == 1 ]]; then
    MARKETING_VERSION="${BASETEN_SWITCH_MARKETING_VERSION:-}"
    BUILD_NUMBER="${BASETEN_SWITCH_BUILD_NUMBER:-}"
    RELEASE_SIGNING_MODE="${BASETEN_SWITCH_RELEASE_SIGNING_MODE:-}"
    [[ "$RELEASE_SIGNING_MODE" == "adhoc" ]] \
        || fail "release build requires BASETEN_SWITCH_RELEASE_SIGNING_MODE=adhoc"
    [[ "$MARKETING_VERSION" =~ ^[0-9]+(\.[0-9]+){1,2}$ ]] \
        || fail "release build requires BASETEN_SWITCH_MARKETING_VERSION as two or three period-separated integers"
    [[ "$BUILD_NUMBER" =~ ^[0-9]+(\.[0-9]+)*$ ]] \
        || fail "release build requires BASETEN_SWITCH_BUILD_NUMBER as period-separated integers"
else
    MARKETING_VERSION="${BASETEN_SWITCH_MARKETING_VERSION:-0.0.0}"
    BUILD_NUMBER="${BASETEN_SWITCH_BUILD_NUMBER:-0}"
    [[ "$MARKETING_VERSION" =~ ^[0-9]+(\.[0-9]+){1,2}$ ]] \
        || fail "BASETEN_SWITCH_MARKETING_VERSION must contain two or three period-separated integers"
    [[ "$BUILD_NUMBER" =~ ^[0-9]+(\.[0-9]+)*$ ]] \
        || fail "BASETEN_SWITCH_BUILD_NUMBER must contain period-separated integers"
fi
if [[ -n "${BASETEN_SWITCH_SWIFT_BUILD_FLAGS:-}" ]]; then
    SWIFT_BUILD_FLAGS=()
    read -r -a SWIFT_BUILD_FLAGS <<< "$BASETEN_SWITCH_SWIFT_BUILD_FLAGS"
fi

# Stable and Preview share the packaged Baseten-green app artwork. AppIcon.svg
# is its editable source; AppIcon.icns is the checked-in macOS bundle asset.
APP_ICON_SOURCE="Assets/AppIcon.icns"

echo "==> swift build -c release (${BUILD_CHANNEL}, universal: arm64 + x86_64)"
if [[ -n "${BASETEN_SWITCH_SWIFT_BUILD_FLAGS:-}" ]]; then
    swift build -c release --arch arm64 --arch x86_64 "${SWIFT_BUILD_FLAGS[@]}"
else
    swift build -c release --arch arm64 --arch x86_64
fi
# With multiple --arch flags SwiftPM lipos the product into the
# .build/apple/Products/Release layout; --show-bin-path with the same
# arches resolves that path instead of hardcoding it.
if [[ -n "${BASETEN_SWITCH_SWIFT_BUILD_FLAGS:-}" ]]; then
    BIN_DIR="$(swift build -c release --arch arm64 --arch x86_64 \
        "${SWIFT_BUILD_FLAGS[@]}" --show-bin-path)"
else
    BIN_DIR="$(swift build -c release --arch arm64 --arch x86_64 --show-bin-path)"
fi
BIN="${BIN_DIR}/BasetenSwitch"
test -x "$BIN" || { echo "error: release binary not found at $BIN" >&2; exit 1; }

APP="${DIST_DIR}/${APP_NAME}.app"
echo "==> assembling ${APP} (marketing ${MARKETING_VERSION}, build ${BUILD_NUMBER})"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/${EXECUTABLE_NAME}"
chmod 755 "$APP/Contents/MacOS/${EXECUTABLE_NAME}"
cp "$APP_ICON_SOURCE" "$APP/Contents/Resources/AppIcon.icns"
cp "Assets/baseten-logo-white.svg" "$APP/Contents/Resources/baseten-logo-white.svg"
cp "Assets/openai-blossom.svg" "$APP/Contents/Resources/openai-blossom.svg"

cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>${BUNDLE_ID}</string>
	<key>CFBundleName</key>
	<string>${APP_NAME}</string>
	<key>CFBundleDisplayName</key>
	<string>${APP_NAME}</string>
	<key>CFBundleExecutable</key>
	<string>${EXECUTABLE_NAME}</string>
	<key>BasetenSwitchBuildChannel</key>
	<string>${BUILD_CHANNEL}</string>
	<!-- Advertises the headless --unregister-login-item one-shot mode:
	     only the app itself can unregister its SMAppService login item,
	     so `baseten-switch uninstall` checks this marker before driving
	     the bundled executable; bundles without it get the manual
	     removal instructions. -->
	<key>BasetenSwitchLoginItemCLI</key>
	<true/>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleShortVersionString</key>
	<string>${MARKETING_VERSION}</string>
	<key>CFBundleVersion</key>
	<string>${BUILD_NUMBER}</string>
	<key>LSMinimumSystemVersion</key>
	<string>13.0</string>
	<key>CFBundleIconFile</key>
	<string>AppIcon</string>
	<key>LSUIElement</key>
	<true/>
	<key>NSHighResolutionCapable</key>
	<true/>
	<!-- The Reauthenticate button sends an Apple event to Terminal via
	     osascript; without this key macOS 13+ denies Automation consent
	     with no prompt (errAEEventNotPermitted, -1743) in the packaged
	     bundle, while a bare dev binary inherits the invoking terminal's
	     consent and masks the failure. -->
	<key>NSAppleEventsUsageDescription</key>
	<string>${APP_NAME} opens Terminal to run 'baseten auth login' when the shared Baseten CLI credential needs reauthentication.</string>
</dict>
</plist>
EOF

if [[ "$RELEASE_BUILD" == 1 ]]; then
    echo "==> codesign (ad-hoc beta release)"
    codesign --force --sign - "$APP/Contents/MacOS/${EXECUTABLE_NAME}"
    codesign --force --sign - "$APP"
else
    echo "==> codesign (development-only ad hoc signature)"
    codesign --force --sign - "$APP"
fi

echo "==> validating"
plutil -lint "$APP/Contents/Info.plist"
# The Automation usage string must survive plist edits: without it the
# packaged app's Reauthenticate button is denied silently (see comment
# in the heredoc above).
plutil -extract NSAppleEventsUsageDescription raw "$APP/Contents/Info.plist" >/dev/null
codesign --verify --deep --strict --verbose=2 "$APP"
if [[ "$RELEASE_BUILD" == 1 ]]; then
    signature_info="$(codesign --display --verbose=4 "$APP" 2>&1)"
    grep -qxF 'Signature=adhoc' <<<"$signature_info" \
        || fail "${APP} does not have the required ad-hoc beta signature"
    executable_signature_info="$(
        codesign --display --verbose=4 "$APP/Contents/MacOS/${EXECUTABLE_NAME}" 2>&1
    )"
    grep -qxF 'Signature=adhoc' <<<"$executable_signature_info" \
        || fail "${APP} executable does not have the required ad-hoc beta signature"
fi
test -x "$APP/Contents/MacOS/${EXECUTABLE_NAME}"
test -s "$APP/Contents/Resources/AppIcon.icns"
test -s "$APP/Contents/Resources/baseten-logo-white.svg"
test -s "$APP/Contents/Resources/openai-blossom.svg"
for check in \
    "CFBundleIdentifier:${BUNDLE_ID}" \
    "CFBundleName:${APP_NAME}" \
    "CFBundleDisplayName:${APP_NAME}" \
    "CFBundleExecutable:${EXECUTABLE_NAME}" \
    "BasetenSwitchBuildChannel:${BUILD_CHANNEL}" \
    "CFBundleShortVersionString:${MARKETING_VERSION}" \
    "CFBundleVersion:${BUILD_NUMBER}"; do
    key="${check%%:*}"
    want="${check#*:}"
    got="$(plutil -extract "$key" raw "$APP/Contents/Info.plist")"
    [[ "$got" == "$want" ]] \
        || fail "${APP} ${key} is '${got}', want '${want}'"
done
# Both slices must actually be in the packaged binary: a thin arm64
# build here is exactly the regression that shipped Intel users a keg
# with no usable app.
ARCHS="$(lipo -archs "$APP/Contents/MacOS/${EXECUTABLE_NAME}")"
echo "archs: ${ARCHS}"
for want in arm64 x86_64; do
    case " $ARCHS " in
        *" $want "*) ;;
        *) echo "error: binary is missing the $want slice (archs: $ARCHS)" >&2; exit 1 ;;
    esac
done
# The stable fallback bundle id and login item menu label must remain
# compiled into the shared Swift product. App identity for packaged
# variants comes from the validated Info.plist above. grep -a scans the
# Mach-O directly (macOS strings(1) misses Swift constants).
for needle in "co.baseten.switch" "Start at Login"; do
    if ! grep -qaF "$needle" "$APP/Contents/MacOS/${EXECUTABLE_NAME}"; then
        echo "error: binary does not embed expected string: $needle" >&2
        exit 1
    fi
done

echo "OK: $(pwd)/${APP}"
