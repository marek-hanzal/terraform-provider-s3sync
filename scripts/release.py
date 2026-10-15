#!/usr/bin/env python3
"""Build immutable provider artifacts locally and publish them without CI."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
REPOSITORY = "marek-hanzal/terraform-provider-s3sync"
PLATFORMS = ("darwin_amd64", "darwin_arm64", "linux_amd64")


def run(*args, capture=False, env=None):
    result = subprocess.run(args, cwd=ROOT, env=env, check=True,
                            text=True, stdout=subprocess.PIPE if capture else None)
    return result.stdout.strip() if capture else None


def version():
    value = (ROOT / "VERSION").read_text().strip()
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?", value):
        raise SystemExit("VERSION must contain a semantic version.")
    return value


def source():
    if run("git", "status", "--porcelain", capture=True):
        raise SystemExit("Commit the source before building or publishing immutable release artifacts.")
    return run("git", "rev-parse", "HEAD", capture=True)


def build():
    value, commit = version(), source()
    output = ROOT / "build" / "releases" / value
    output.mkdir(parents=True, exist_ok=True)
    flags = ["-mod=readonly", "-trimpath", "-buildvcs=false",
             f"-ldflags=-s -w -buildid= -X main.version={value}"]
    artifacts, hashes = [], {}
    for platform in PLATFORMS:
        target_os, arch = platform.split("_")
        name = f"terraform-provider-s3sync_v{value}_{platform}"
        destination = output / name
        temporary = output / (name + ".tmp")
        env = dict(os.environ, CGO_ENABLED="0", GOOS=target_os, GOARCH=arch,
                   GOTOOLCHAIN="local")
        run("go", "build", *flags, "-o", str(temporary), ".", env=env)
        body = temporary.read_bytes()
        digest = hashlib.sha256(body).hexdigest()
        if destination.exists() and destination.read_bytes() != body:
            temporary.unlink()
            raise SystemExit(f"Refusing different bytes under existing artifact {name}.")
        temporary.replace(destination)
        destination.chmod(0o755)
        hashes[platform] = digest
        artifacts.append({"name": name, "platform": platform,
                          "sha256": digest, "size": len(body)})
    manifest = {"source": "marek-hanzal/s3sync", "version": value,
                "commit": commit, "repository": f"https://github.com/{REPOSITORY}",
                "release": f"v{value}", "sha256": hashes}
    provenance = {"repository": f"https://github.com/{REPOSITORY}",
                  "commit": commit, "version": value,
                  "go": run("go", "version", capture=True), "flags": flags,
                  "cgo_enabled": False, "artifacts": artifacts}
    (output / "provider-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (output / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
    (output / "SHA256SUMS").write_text("".join(
        f"{item['sha256']}  {item['name']}\n" for item in artifacts))
    print(output)
    return output


def ensure_tag(tag, commit, value):
    remote = run("git", "ls-remote", "--tags", "origin", f"refs/tags/{tag}",
                 f"refs/tags/{tag}^{{}}", capture=True)
    refs = dict(line.split()[::-1] for line in remote.splitlines())
    remote_commit = refs.get(f"refs/tags/{tag}^{{}}", refs.get(f"refs/tags/{tag}"))
    local = subprocess.run(["git", "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}"],
                           cwd=ROOT, text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if (local.returncode == 0 and local.stdout.strip() != commit) or (remote_commit and remote_commit != commit):
        raise SystemExit("Existing release tag belongs to a different source commit.")
    if remote_commit:
        run("git", "push", "origin", "HEAD:main")
        return
    if local.returncode != 0:
        run("git", "tag", "-a", tag, "-m", f"S3 Sync provider {value}")
    run("git", "push", "--atomic", "origin", "HEAD:main", f"refs/tags/{tag}")


def release_for(tag):
    # Listing distinguishes an absent release from authentication/network failure.
    pages = json.loads(run("gh", "api", f"repos/{REPOSITORY}/releases",
                           "--paginate", "--slurp", capture=True))
    return next((release for page in pages for release in page if release["tag_name"] == tag), None)


def verify_assets(tag, release, expected):
    names = [asset["name"] for asset in release["assets"]]
    if len(names) != len(set(names)) or set(names) - expected.keys():
        raise SystemExit("Release contains unexpected or duplicate assets; refusing mutation.")
    if names:
        with tempfile.TemporaryDirectory(prefix="s3sync-release-") as directory:
            run("gh", "release", "download", tag, "--repo", REPOSITORY,
                "--dir", directory, "--pattern", "*")
            for name in names:
                if hashlib.sha256((Path(directory) / name).read_bytes()).hexdigest() != expected[name]:
                    raise SystemExit(f"Existing remote artifact {name} has different bytes; refusing replacement.")
    return set(names)


def publish():
    value, commit = version(), source()
    output = ROOT / "build" / "releases" / value
    manifest = json.loads((output / "provider-manifest.json").read_text())
    if manifest["commit"] != commit or manifest["version"] != value:
        raise SystemExit("Release artifacts do not belong to this clean source commit.")
    if set(manifest["sha256"]) != set(PLATFORMS):
        raise SystemExit("Release platform manifest is incomplete.")
    for platform, digest in manifest["sha256"].items():
        path = output / f"terraform-provider-s3sync_v{value}_{platform}"
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise SystemExit("Release artifact verification failed.")
    tag = f"v{value}"
    assets = [output / f"terraform-provider-s3sync_v{value}_{p}" for p in PLATFORMS]
    assets += [output / name for name in ("SHA256SUMS", "provenance.json", "provider-manifest.json")]
    expected = {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in assets}
    ensure_tag(tag, commit, value)
    release = release_for(tag)
    notes = ROOT / "docs" / "release-notes.md"
    if release is None:
        run("gh", "release", "create", tag, "--repo", REPOSITORY,
            "--verify-tag", "--draft", "--prerelease", "--title", f"S3 Sync prototype {value}",
            "--notes-file", str(notes))
        release = release_for(tag)
    if release is None or release["body"].strip() != notes.read_text().strip():
        raise SystemExit("Remote release notes do not match the source release notes.")
    existing = verify_assets(tag, release, expected)
    missing = [str(path) for path in assets if path.name not in existing]
    if not release["draft"]:
        if missing:
            raise SystemExit("Published release is incomplete; refusing mutation.")
        print(f"Release {tag} already published and verified.")
        return
    if missing:
        run("gh", "release", "upload", tag, *missing, "--repo", REPOSITORY)
    release = release_for(tag)
    if release is None or verify_assets(tag, release, expected) != expected.keys():
        raise SystemExit("Draft release is incomplete; it remains unpublished.")
    run("gh", "release", "edit", tag, "--repo", REPOSITORY, "--draft=false", "--prerelease")


if __name__ == "__main__":
    if len(sys.argv) != 2 or sys.argv[1] not in ("build", "publish"):
        raise SystemExit("Usage: release.py build|publish")
    build() if sys.argv[1] == "build" else publish()
