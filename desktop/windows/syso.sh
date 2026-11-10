#!/bin/sh
# Generates desktop/rsrc_windows_<arch>.syso: the icon, version info and
# manifest that `go build` links into the Windows executable. Run by
# `make desktop` when the target is Windows, before the build.
set -e
cd "$(dirname "$0")/.."
# The release workflow passes the tag's version.
VERSION=${CAT_VERSION:-0.1.0}
ARCH=$(go env GOARCH)
# The generator comes from the Wails version the app is built against. It
# runs on the host, so a cross-build's GOOS/GOARCH must not reach it.
WAILS=$(go list -m -f '{{.Version}}' github.com/wailsapp/wails/v3)
wails3() { env -u GOOS -u GOARCH go run "github.com/wailsapp/wails/v3/cmd/wails3@$WAILS" "$@"; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
wails3 generate icons -input assets/appicon.png -windowsfilename "$TMP/cat.ico" -macfilename "$TMP/cat.icns"
sed "s/@VERSION@/$VERSION/" windows/cat.exe.manifest > "$TMP/cat.exe.manifest"
cat > "$TMP/info.json" <<JSON
{
  "fixed": { "file_version": "$VERSION", "product_version": "$VERSION" },
  "info": {
    "0409": {
      "FileVersion": "$VERSION",
      "ProductVersion": "$VERSION",
      "FileDescription": "Cat",
      "ProductName": "Cat",
      "LegalCopyright": "MIT License"
    }
  }
}
JSON
rm -f rsrc_windows_*.syso
wails3 generate syso -arch "$ARCH" -icon "$TMP/cat.ico" -manifest "$TMP/cat.exe.manifest" \
  -info "$TMP/info.json" -out "rsrc_windows_$ARCH.syso"
echo "generated desktop/rsrc_windows_$ARCH.syso"
