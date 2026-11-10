#!/usr/bin/env python3
"""Builds every plugin for every platform, zips each with its plugin.json and
writes a market index with checksums.

    python3 scripts/build-market.py https://example.com/cat-market

The argument is the URL the output directory (dist/market) will be served
from; host that directory there and start Cat with --market <url>/index.json.
"""
import hashlib, json, os, subprocess, sys, zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "dist", "market")
PLATFORMS = [("darwin", "arm64"), ("darwin", "amd64"), ("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64")]
BUN_TARGET = {("darwin", "arm64"): "bun-darwin-arm64", ("darwin", "amd64"): "bun-darwin-x64",
              ("linux", "amd64"): "bun-linux-x64", ("linux", "arm64"): "bun-linux-arm64",
              ("windows", "amd64"): "bun-windows-x64"}


def build(plugin_dir, pid, goos, goarch, out):
    if os.path.exists(os.path.join(plugin_dir, "main.go")):
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", out, "./plugins/" + pid], cwd=ROOT, env=env, check=True)
    elif os.path.exists(os.path.join(plugin_dir, "package.json")):
        subprocess.run(["bun", "install", "--silent"], cwd=plugin_dir, check=True)
        subprocess.run(["bun", "build", "src/index.ts", "--compile", "--target=" + BUN_TARGET[(goos, goarch)], "--outfile", out],
                       cwd=plugin_dir, check=True, stdout=subprocess.DEVNULL)
    else:
        raise SystemExit(f"don't know how to build {pid}")


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    base = sys.argv[1].rstrip("/")
    # Optional: limit to some platforms, e.g. `... darwin/arm64 linux/amd64`.
    only = set(sys.argv[2:])
    platforms = [p for p in PLATFORMS if not only or "/".join(p) in only]
    os.makedirs(OUT, exist_ok=True)
    entries = []
    plugins_root = os.path.join(ROOT, "plugins")
    for pid in sorted(os.listdir(plugins_root)):
        plugin_dir = os.path.join(plugins_root, pid)
        manifest_path = os.path.join(plugin_dir, "plugin.json")
        if not os.path.exists(manifest_path):
            continue
        manifest = json.load(open(manifest_path, encoding="utf-8"))
        entry = {"id": pid, "name": manifest["name"], "version": manifest["version"],
                 "description": manifest.get("description", ""), "platforms": {}}
        for goos, goarch in platforms:
            exe = f"cat-plugin-{pid}" + (".exe" if goos == "windows" else "")
            tmp = os.path.join(OUT, f".build-{exe}")
            print(f"build {pid} {goos}/{goarch}", flush=True)
            build(plugin_dir, pid, goos, goarch, tmp)
            # On Windows the manifest's command needs the .exe suffix.
            m = dict(manifest)
            m["command"] = ["./" + exe] + manifest["command"][1:]
            name = f"{pid}-{manifest['version']}-{goos}-{goarch}.zip"
            with zipfile.ZipFile(os.path.join(OUT, name), "w", zipfile.ZIP_DEFLATED) as z:
                z.writestr("plugin.json", json.dumps(m, ensure_ascii=False, indent=2))
                info = zipfile.ZipInfo(exe)
                info.external_attr = 0o755 << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                z.writestr(info, open(tmp, "rb").read())
            os.remove(tmp)
            digest = hashlib.sha256(open(os.path.join(OUT, name), "rb").read()).hexdigest()
            entry["platforms"][f"{goos}/{goarch}"] = {"url": f"{base}/{name}", "sha256": digest, "format": "zip"}
        entries.append(entry)
    json.dump({"plugins": entries}, open(os.path.join(OUT, "index.json"), "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print(f"wrote {len(entries)} plugins to {OUT}")


if __name__ == "__main__":
    main()
