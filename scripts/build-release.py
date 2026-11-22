#!/usr/bin/env python3
"""Packages catd with the official plugins, one archive per platform.

    python3 scripts/build-release.py v0.1.0 [darwin/arm64 linux/amd64 ...]

Run after the web UI is built (`make frontend`, catd embeds it). Output goes
to dist/release:

    catd-<version>-<os>-<arch>.tar.gz  (.zip on Windows)
        catd
        plugins/<id>/plugin.json, cat-plugin-<id>
    SHA256SUMS

catd looks for plugins in the directory beside itself, so an unpacked archive
runs as it is.
"""
import hashlib, json, os, shutil, subprocess, sys, tarfile, zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "dist", "release")
PLATFORMS = [("darwin", "arm64"), ("darwin", "amd64"), ("linux", "amd64"), ("linux", "arm64"), ("windows", "amd64")]
BUN_TARGET = {("darwin", "arm64"): "bun-darwin-arm64", ("darwin", "amd64"): "bun-darwin-x64",
              ("linux", "amd64"): "bun-linux-x64", ("linux", "arm64"): "bun-linux-arm64",
              ("windows", "amd64"): "bun-windows-x64"}


def discover_plugins():
    plugins_root = os.path.join(ROOT, "plugins")
    found = []
    for pid in sorted(os.listdir(plugins_root)):
        plugin_dir = os.path.join(plugins_root, pid)
        manifest_path = os.path.join(plugin_dir, "plugin.json")
        if not os.path.exists(manifest_path):
            continue
        manifest = json.load(open(manifest_path, encoding="utf-8"))
        found.append((pid, plugin_dir, manifest))
    return found


def build_plugin(plugin_dir, pid, goos, goarch, out):
    if os.path.exists(os.path.join(plugin_dir, "main.go")):
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", out, "./plugins/" + pid],
                       cwd=ROOT, env=env, check=True)
    elif os.path.exists(os.path.join(plugin_dir, "package.json")):
        subprocess.run(["bun", "build", "src/index.ts", "--compile", "--target=" + BUN_TARGET[(goos, goarch)], "--outfile", out],
                       cwd=plugin_dir, check=True, stdout=subprocess.DEVNULL)
    else:
        raise SystemExit(f"don't know how to build {pid}")


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    version = sys.argv[1]
    only = set(sys.argv[2:])
    platforms = [p for p in PLATFORMS if not only or "/".join(p) in only]
    if not platforms:
        raise SystemExit(f"no matching platforms: {', '.join(sorted(only))}")
    if not os.path.exists(os.path.join(ROOT, "frontend", "dist", "index.html")):
        raise SystemExit("frontend/dist is empty: run `make frontend` first")

    plugins = discover_plugins()
    for _, plugin_dir, _ in plugins:
        if os.path.exists(os.path.join(plugin_dir, "package.json")):
            subprocess.run(["bun", "install", "--silent"], cwd=plugin_dir, check=True)

    shutil.rmtree(OUT, ignore_errors=True)
    os.makedirs(OUT)
    archives = []
    for goos, goarch in platforms:
        name = f"catd-{version}-{goos}-{goarch}"
        stage = os.path.join(OUT, name)
        os.makedirs(stage, exist_ok=True)
        print(f"package {name}", flush=True)
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
        exe = "catd" + (".exe" if goos == "windows" else "")
        subprocess.run(["go", "build", "-trimpath", "-ldflags",
                        "-s -w -X github.com/dounx/cat/app.Version=" + version.removeprefix("v"),
                        "-o", os.path.join(stage, exe), "./cmd/catd"],
                       cwd=ROOT, env=env, check=True)
        for pid, plugin_dir, manifest in plugins:
            dest = os.path.join(stage, "plugins", pid)
            os.makedirs(dest, exist_ok=True)
            pexe = f"cat-plugin-{pid}" + (".exe" if goos == "windows" else "")
            out_bin = os.path.join(dest, pexe)
            print(f"  plugin {pid} {goos}/{goarch}", flush=True)
            build_plugin(plugin_dir, pid, goos, goarch, out_bin)
            if goos != "windows":
                os.chmod(out_bin, 0o755)
            # On Windows the manifest's command needs the .exe suffix.
            m = dict(manifest)
            m["command"] = ["./" + pexe] + manifest["command"][1:]
            with open(os.path.join(dest, "plugin.json"), "w", encoding="utf-8") as mf:
                json.dump(m, mf, ensure_ascii=False, indent=2)
        if goos == "windows":
            archive = name + ".zip"
            with zipfile.ZipFile(os.path.join(OUT, archive), "w", zipfile.ZIP_DEFLATED) as z:
                for dirpath, _, files in sorted(os.walk(stage)):
                    for f in sorted(files):
                        full = os.path.join(dirpath, f)
                        z.write(full, os.path.relpath(full, OUT))
        else:
            archive = name + ".tar.gz"
            with tarfile.open(os.path.join(OUT, archive), "w:gz") as t:
                t.add(stage, arcname=name)
        shutil.rmtree(stage)
        archives.append(archive)

    with open(os.path.join(OUT, "SHA256SUMS"), "w") as sums:
        for archive in archives:
            digest = hashlib.sha256(open(os.path.join(OUT, archive), "rb").read()).hexdigest()
            sums.write(f"{digest}  {archive}\n")
    print(f"wrote {len(archives)} archives to {OUT}")


if __name__ == "__main__":
    main()
