#!/usr/bin/env python3
"""Packages catd with the official plugins, one archive per platform.

    python3 scripts/build-release.py v0.1.0

Run after the web UI is built (`make frontend`, catd embeds it) and after
scripts/build-market.py, whose plugin archives in dist/market are unpacked
into each package. Output goes to dist/release:

    cat-<version>-<os>-<arch>.tar.gz  (.zip on Windows)
        catd
        plugins/<id>/plugin.json, cat-plugin-<id>
    SHA256SUMS

catd looks for plugins in the directory beside itself, so an unpacked archive
runs as it is.
"""
import hashlib, json, os, shutil, subprocess, sys, tarfile, zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MARKET = os.path.join(ROOT, "dist", "market")
OUT = os.path.join(ROOT, "dist", "release")


def main():
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    version = sys.argv[1]
    if not os.path.exists(os.path.join(ROOT, "frontend", "dist", "index.html")):
        raise SystemExit("frontend/dist is empty: run `make frontend` first")
    index_path = os.path.join(MARKET, "index.json")
    if not os.path.exists(index_path):
        raise SystemExit("dist/market/index.json not found: run scripts/build-market.py first")
    plugins = json.load(open(index_path, encoding="utf-8"))["plugins"]
    platforms = sorted({p for entry in plugins for p in entry["platforms"]})

    shutil.rmtree(OUT, ignore_errors=True)
    os.makedirs(OUT)
    archives = []
    for platform in platforms:
        goos, goarch = platform.split("/")
        name = f"cat-{version}-{goos}-{goarch}"
        stage = os.path.join(OUT, name)
        print(f"package {name}", flush=True)
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
        exe = "catd" + (".exe" if goos == "windows" else "")
        subprocess.run(["go", "build", "-trimpath", "-ldflags", "-s -w", "-o", os.path.join(stage, exe), "./cmd/catd"],
                       cwd=ROOT, env=env, check=True)
        for entry in plugins:
            artifact = entry["platforms"].get(platform)
            if not artifact:
                continue
            dest = os.path.join(stage, "plugins", entry["id"])
            with zipfile.ZipFile(os.path.join(MARKET, os.path.basename(artifact["url"]))) as z:
                z.extractall(dest)
            # zipfile drops the executable bit.
            for f in os.listdir(dest):
                if f.startswith("cat-plugin-"):
                    os.chmod(os.path.join(dest, f), 0o755)
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
