#!/bin/sh
# Builds bin/Cat-linux from bin/cat-desktop, the app icon and the plugins
# built in the source tree: a folder that runs from wherever it is unpacked.
# Also builds native Linux packages when the corresponding tools are available:
#   - bin/cat_<version>_<arch>.deb   (dpkg-deb)
#   - bin/cat_<version>_<arch>.rpm   (rpmbuild)
#   - bin/Cat.AppImage               (appimagetool)
# Run through `make app`.
set -e
cd "$(dirname "$0")/.."
OUT=bin/Cat-linux
# The release workflow passes the tag's version.
VERSION=${CAT_VERSION:-0.1.0}
rm -rf "$OUT" bin/cat_*.deb bin/cat_*.rpm bin/Cat.AppImage
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

DEB=""
RPM_TOP=""
APPDIR_ROOT=""
trap 'rm -rf "$DEB" "$RPM_TOP" "$APPDIR_ROOT"' EXIT

if command -v dpkg-deb >/dev/null; then
  DEB=$(mktemp -d)
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
fi

if command -v rpmbuild >/dev/null; then
  RPM_TOP=$(mktemp -d)
  mkdir -p "$RPM_TOP/BUILD" "$RPM_TOP/RPMS" "$RPM_TOP/SOURCES" "$RPM_TOP/SPECS" "$RPM_TOP/SRPMS"
  RPM_VER=$(printf '%s' "$VERSION" | tr '-' '~')
  RPM_ARCH=$(uname -m)
  SRC_ABS="$(pwd)/$OUT"
  cat > "$RPM_TOP/SPECS/cat.spec" <<SPEC
Name:           cat-desktop
Version:        $RPM_VER
Release:        1
Summary:        Cat event hub
License:        MIT
URL:            https://github.com/Dounx/cat
AutoReqProv:    no
Requires:       libgtk-4.so.1()(64bit)
Requires:       libwebkitgtk-6.0.so.4()(64bit)
%define _build_id_links none
%define debug_package %{nil}
%define __os_install_post %{nil}

%description
Receives events from chat channels, feeds and monitors, and runs rules on
them. The desktop app keeps the hub running in the system tray.

%install
mkdir -p %{buildroot}/opt/cat %{buildroot}/usr/bin %{buildroot}/usr/share/applications %{buildroot}/usr/share/pixmaps
cp -a "$SRC_ABS/." %{buildroot}/opt/cat/
rm -f %{buildroot}/opt/cat/install.sh
mv %{buildroot}/opt/cat/cat.png %{buildroot}/usr/share/pixmaps/cat-desktop.png
ln -s /opt/cat/cat-desktop %{buildroot}/usr/bin/cat-desktop
cat > %{buildroot}/usr/share/applications/cat-desktop.desktop <<'ENTRY'
[Desktop Entry]
Type=Application
Name=Cat
Comment=Cat event hub
Exec=/opt/cat/cat-desktop
Icon=cat-desktop
Terminal=false
Categories=Utility;
ENTRY

%files
/opt/cat
/usr/bin/cat-desktop
/usr/share/applications/cat-desktop.desktop
/usr/share/pixmaps/cat-desktop.png
SPEC
  rpmbuild --quiet --define "_topdir $RPM_TOP" --target "$RPM_ARCH" -bb "$RPM_TOP/SPECS/cat.spec"
  cp "$RPM_TOP/RPMS/$RPM_ARCH/"*.rpm "bin/cat_${VERSION}_${RPM_ARCH}.rpm"
  echo "built bin/cat_${VERSION}_${RPM_ARCH}.rpm"
fi

if command -v appimagetool >/dev/null; then
  APPDIR_ROOT=$(mktemp -d)
  APPDIR="$APPDIR_ROOT/Cat.AppDir"
  mkdir -p "$APPDIR/usr/bin"
  cp -R "$OUT/." "$APPDIR/usr/bin/"
  rm -f "$APPDIR/usr/bin/install.sh"
  mv "$APPDIR/usr/bin/cat.png" "$APPDIR/cat-desktop.png"
  ln -s cat-desktop.png "$APPDIR/.DirIcon"
  cat > "$APPDIR/cat-desktop.desktop" <<ENTRY
[Desktop Entry]
Type=Application
Name=Cat
Comment=Cat event hub
Exec=cat-desktop
Icon=cat-desktop
Terminal=false
Categories=Utility;
ENTRY
  cat > "$APPDIR/AppRun" <<'APPRUN'
#!/bin/sh
HERE=$(dirname "$(readlink -f "$0")")
exec "$HERE/usr/bin/cat-desktop" "$@"
APPRUN
  chmod 755 "$APPDIR/AppRun"
  APPIMAGE_EXTRACT_AND_RUN=1 ARCH=$(uname -m) appimagetool --no-appstream "$APPDIR" bin/Cat.AppImage >/dev/null
  echo "built bin/Cat.AppImage"
fi
