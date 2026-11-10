#!/bin/sh
# Builds bin/Cat-linux from bin/cat-desktop, the app icon and the plugins
# built in the source tree: a folder that runs from wherever it is unpacked.
# Where dpkg-deb is available it also builds bin/cat_<version>_<arch>.deb.
# Run through `make app`.
set -e
cd "$(dirname "$0")/.."
OUT=bin/Cat-linux
# The release workflow passes the tag's version.
VERSION=${CAT_VERSION:-0.1.0}
rm -rf "$OUT" bin/cat_*.deb
mkdir -p "$OUT/plugins"
cp bin/cat-desktop "$OUT/cat-desktop"
cp desktop/assets/appicon.png "$OUT/cat.png"
cp desktop/linux/install.sh "$OUT/install.sh"
chmod 755 "$OUT/cat-desktop" "$OUT/install.sh"

# Plugins live next to the executable, where the hub looks for them.
for dir in plugins/*/; do
  name=$(basename "$dir")
  [ -f "$dir/plugin.json" ] && [ -f "$dir/cat-plugin-$name" ] || continue
  mkdir -p "$OUT/plugins/$name"
  cp "$dir/plugin.json" "$dir/cat-plugin-$name" "$OUT/plugins/$name/"
done
echo "built $OUT"

command -v dpkg-deb >/dev/null || exit 0
DEB=$(mktemp -d)
trap 'rm -rf "$DEB"' EXIT
chmod 755 "$DEB"
mkdir -p "$DEB/DEBIAN" "$DEB/opt" "$DEB/usr/bin" "$DEB/usr/share/applications" "$DEB/usr/share/pixmaps"
cp -R "$OUT" "$DEB/opt/cat"
rm "$DEB/opt/cat/install.sh"
mv "$DEB/opt/cat/cat.png" "$DEB/usr/share/pixmaps/cat-desktop.png"
ln -s /opt/cat/cat-desktop "$DEB/usr/bin/cat-desktop"
cat > "$DEB/usr/share/applications/cat-desktop.desktop" <<ENTRY
[Desktop Entry]
Type=Application
Name=Cat
Comment=Cat event hub
Exec=/opt/cat/cat-desktop
Icon=cat-desktop
Terminal=false
Categories=Utility;
ENTRY
ARCH=$(dpkg --print-architecture)
cat > "$DEB/DEBIAN/control" <<CONTROL
Package: cat-desktop
Version: $VERSION
Architecture: $ARCH
Maintainer: Dounx <noreply@github.com>
Section: utils
Priority: optional
Depends: libgtk-4-1, libwebkitgtk-6.0-4
Homepage: https://github.com/Dounx/cat
Description: Cat event hub
 Receives events from chat channels, feeds and monitors, and runs rules on
 them. The desktop app keeps the hub running in the system tray.
CONTROL
dpkg-deb --root-owner-group --build "$DEB" "bin/cat_${VERSION}_$ARCH.deb" >/dev/null
echo "built bin/cat_${VERSION}_$ARCH.deb"
