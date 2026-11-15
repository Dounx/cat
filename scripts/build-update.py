#!/usr/bin/env python3
"""Writes update.json, which a running Cat reads to find and install a newer
version of itself.

    python3 scripts/build-update.py v0.2.0 <base url> <directory>

The directory (searched with its subdirectories) holds the release's
archives: the desktop app's (Cat-<tag>-<os>-<arch>) and catd's
(cat-<tag>-<os>-<arch>). The base URL is where they will be downloaded from,
by file name. update.json is written into the directory.
"""
import hashlib, json, os, re, sys

NAME = re.compile(r"^(Cat|cat)-.+-(macos|darwin|linux|windows)-(amd64|arm64)\.(zip|tar\.gz)$")


def main():
    if len(sys.argv) != 4:
        raise SystemExit(__doc__)
    tag, base, root = sys.argv[1], sys.argv[2].rstrip("/"), sys.argv[3]
    packages = {"desktop": {}, "catd": {}}
    for dirpath, _, files in sorted(os.walk(root)):
        for f in sorted(files):
            m = NAME.match(f)
            if not m:
                continue
            kind = "desktop" if m.group(1) == "Cat" else "catd"
            platform = m.group(2).replace("macos", "darwin") + "/" + m.group(3)
            digest = hashlib.sha256(open(os.path.join(dirpath, f), "rb").read()).hexdigest()
            packages[kind][platform] = {"url": f"{base}/{f}", "sha256": digest, "format": m.group(4)}
    if not any(packages.values()):
        raise SystemExit(f"no release archives under {root}")
    notes = base.replace("/releases/download/", "/releases/tag/") if "/releases/download/" in base else ""
    manifest = {"version": tag.removeprefix("v"), "notes": notes, "packages": packages}
    with open(os.path.join(root, "update.json"), "w", encoding="utf-8") as out:
        json.dump(manifest, out, ensure_ascii=False, indent=2)
    for kind, platforms in packages.items():
        print(f"{kind}: {', '.join(sorted(platforms)) or 'none'}")


if __name__ == "__main__":
    main()
