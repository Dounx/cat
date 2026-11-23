#!/bin/sh
# Builds bin/Cat-windows from bin/cat-desktop.exe and the plugins built in the
# source tree: a folder that runs from wherever it is unpacked. Run through
# `make app` (in Git Bash on Windows, or with GOOS=windows elsewhere).
set -e
cd "$(dirname "$0")/.."
OUT=bin/Cat-windows
VERSION=${CAT_VERSION:-0.1.0}
rm -rf "$OUT" bin/Cat-setup.exe
mkdir -p "$OUT/plugins"
cp bin/cat-desktop.exe "$OUT/Cat.exe"

# Plugins live next to the executable, where the hub looks for them.
for dir in plugins/*/; do
  name=$(basename "$dir")
  [ -f "$dir/plugin.json" ] && [ -f "$dir/cat-plugin-$name.exe" ] || continue
  mkdir -p "$OUT/plugins/$name"
  cp "$dir/cat-plugin-$name.exe" "$OUT/plugins/$name/"
  # The manifest's command needs the .exe suffix here.
  sed "s|\"\./cat-plugin-$name\"|\"./cat-plugin-$name.exe\"|" "$dir/plugin.json" > "$OUT/plugins/$name/plugin.json"
done
echo "built $OUT"

ISCC=""
for candidate in "$(command -v iscc 2>/dev/null || true)" \
  "/c/Program Files (x86)/Inno Setup 6/ISCC.exe" \
  "/c/Program Files/Inno Setup 6/ISCC.exe"; do
  if [ -n "$candidate" ] && [ -x "$candidate" ]; then
    ISCC="$candidate"
    break
  fi
done
if [ -n "$ISCC" ]; then
  MSYS_NO_PATHCONV=1 "$ISCC" /Q "/DVersion=$VERSION" desktop/windows/installer.iss
  echo "built bin/Cat-setup.exe"
fi
