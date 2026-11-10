#!/bin/sh
# Adds Cat to the application menu, pointing at this folder. Run it again
# after moving the folder; remove ~/.local/share/applications/cat-desktop.desktop
# to undo.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
APPS=${XDG_DATA_HOME:-$HOME/.local/share}/applications
mkdir -p "$APPS"
cat > "$APPS/cat-desktop.desktop" <<ENTRY
[Desktop Entry]
Type=Application
Name=Cat
Comment=Cat event hub
Exec="$HERE/cat-desktop"
Icon=$HERE/cat.png
Terminal=false
Categories=Utility;
ENTRY
echo "installed $APPS/cat-desktop.desktop"
