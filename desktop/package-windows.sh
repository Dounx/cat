#!/bin/sh
# Builds bin/Cat-windows from bin/cat-desktop.exe and the plugins built in the
# source tree: a folder that runs from wherever it is unpacked. Run through
# `make app` (in Git Bash on Windows, or with GOOS=windows elsewhere).
set -e
cd "$(dirname "$0")/.."
OUT=bin/Cat-windows
rm -rf "$OUT"
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
