#!/bin/sh
# Builds bin/Cat.app from bin/cat-desktop, the generated icon and the plugins
# built in the source tree. Run through `make app`.
set -e
cd "$(dirname "$0")/.."
APP=bin/Cat.app
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS/plugins" "$APP/Contents/Resources"
cp bin/cat-desktop "$APP/Contents/MacOS/Cat"

# Plugins live next to the executable, where the hub looks for them.
for dir in plugins/*/; do
  name=$(basename "$dir")
  [ -f "$dir/plugin.json" ] && [ -f "$dir/cat-plugin-$name" ] || continue
  mkdir -p "$APP/Contents/MacOS/plugins/$name"
  cp "$dir/plugin.json" "$dir/cat-plugin-$name" "$APP/Contents/MacOS/plugins/$name/"
done

ICONSET=$(mktemp -d)/Cat.iconset
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
  sips -z $size $size desktop/assets/appicon.png --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  sips -z $((size * 2)) $((size * 2)) desktop/assets/appicon.png --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/Cat.icns"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Cat</string>
  <key>CFBundleDisplayName</key><string>Cat</string>
  <key>CFBundleIdentifier</key><string>com.dounx.cat</string>
  <key>CFBundleExecutable</key><string>Cat</string>
  <key>CFBundleIconFile</key><string>Cat</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>CFBundleVersion</key><string>0.1.0</string>
  <key>LSMinimumSystemVersion</key><string>11.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict></plist>
PLIST
codesign --force --deep --sign - "$APP" 2>/dev/null || true
echo "built $APP"
