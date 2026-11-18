#!/usr/bin/env bash
# Cross-compiles magic for Windows (amd64) and macOS (Apple Silicon) and
# wraps the macOS binary in Magic.app. Needs Go 1.26; no C toolchain.
set -euo pipefail

cd "$(dirname "$0")"
export CGO_ENABLED=0
flags=(-trimpath -ldflags "-s -w")
pkg=./cmd/magic

rm -rf dist
mkdir -p dist

GOOS=windows GOARCH=amd64 go build "${flags[@]}" -o dist/magic-windows-amd64.exe "$pkg"
GOOS=darwin GOARCH=arm64 go build "${flags[@]}" -o dist/magic-darwin-arm64 "$pkg"

app=dist/Magic.app/Contents
mkdir -p "$app/MacOS"
cp dist/magic-darwin-arm64 "$app/MacOS/magic"
cat >"$app/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>com.local.magic</string>
	<key>CFBundleName</key>
	<string>Magic</string>
	<key>CFBundleExecutable</key>
	<string>magic</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>LSMinimumSystemVersion</key>
	<string>14.0</string>
	<key>NSHighResolutionCapable</key>
	<true/>
</dict>
</plist>
EOF

echo "built:"
ls -l dist dist/Magic.app/Contents/MacOS
