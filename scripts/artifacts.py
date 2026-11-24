"""Build metadata and store-compatible ZIP/checksum files. Uses only the stdlib."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import zipfile


PLUGIN_ID = "vibe-bridge"
PLATFORMS = (
    ("linux", "amd64", "ubuntu-22.04", "gcc", "python3"),
    ("linux", "arm64", "ubuntu-22.04-arm", "gcc", "python3"),
    ("darwin", "amd64", "macos-15-intel", "clang", "python3"),
    ("darwin", "arm64", "macos-15", "clang", "python3"),
    ("windows", "amd64", "windows-2022", "gcc", "python"),
    ("windows", "arm64", "windows-11-arm", "clang", "python"),
)
EXTENSIONS = {"linux": ".so", "darwin": ".dylib", "windows": ".dll"}


def release_version(tag):
    if not re.fullmatch(r"v[0-9]+(?:\.[0-9]+)+", tag):
        raise ValueError("Release tags must be dotted numeric versions, such as v0.2.1")
    return tag[1:]


def validate_version(version):
    if not re.fullmatch(r"(?:[0-9]+(?:\.[0-9]+)+|dev-[0-9a-f]{12})", version):
        raise ValueError(f"Invalid artifact version: {version!r}")


def build_version(ref_type, ref_name, sha):
    if ref_type == "tag":
        return release_version(ref_name)
    if ref_type != "branch" or ref_name != "main" or not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("Expected a main push or a version tag push")
    return f"dev-{sha[:12]}"


def library_name(goos):
    return PLUGIN_ID + EXTENSIONS[goos]


def asset_name(version, goos, goarch):
    validate_version(version)
    if (goos, goarch) not in {(p[0], p[1]) for p in PLATFORMS}:
        raise ValueError(f"Unsupported platform: {goos}/{goarch}")
    return f"{PLUGIN_ID}_{version}_{goos}_{goarch}.zip"


def metadata():
    version = build_version(os.environ["GITHUB_REF_TYPE"], os.environ["GITHUB_REF_NAME"], os.environ["GITHUB_SHA"])
    matrix = []
    for goos, goarch, runner, cc, python in PLATFORMS:
        cpu = "x86-64" if goarch == "amd64" else "armv8-a"
        matrix.append(dict(goos=goos, goarch=goarch, runner=runner, cc=cc, python=python,
                           library=library_name(goos), cflags=f"-O2 -g -march={cpu}"))
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8", newline="\n") as output:
        output.write(f"version={version}\n")
        output.write("matrix=" + json.dumps({"include": matrix}, separators=(",", ":")) + "\n")
    print(f"Build version: {version}")


def verify_archive(path, goos):
    with zipfile.ZipFile(path) as archive:
        if archive.namelist() != [library_name(goos)]:
            raise ValueError(f"{path.name}: ZIP must contain only {library_name(goos)} at its root")
        entry = archive.infolist()[0]
        if entry.file_size == 0 or entry.is_dir() or (entry.external_attr >> 16) & 0o170000 != 0o100000:
            raise ValueError(f"{path.name}: library must be a nonempty regular file")
        if archive.testzip() is not None:
            raise ValueError(f"{path.name}: corrupt ZIP content")


def package(version, goos, goarch, library, output):
    name = asset_name(version, goos, goarch)
    library = Path(library)
    if library.name != library_name(goos) or not library.is_file() or library.is_symlink():
        raise ValueError(f"Expected a regular {library_name(goos)} file")
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    path = output / name
    # Fix ZIP metadata so rerunning a build does not change checksums via file timestamps.
    entry = zipfile.ZipInfo(library.name, date_time=(1980, 1, 1, 0, 0, 0))
    entry.create_system = 3
    entry.external_attr = 0o100755 << 16
    entry.compress_type = zipfile.ZIP_DEFLATED
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        archive.writestr(entry, library.read_bytes())
    verify_archive(path, goos)
    print(path)
    return path


def checksum_content(version, output):
    output = Path(output)
    expected = {asset_name(version, p[0], p[1]): p[0] for p in PLATFORMS}
    actual = {path.name for path in output.iterdir() if path.name != "checksums.txt"}
    if actual != set(expected):
        raise ValueError(f"Incomplete or unexpected artifacts. Missing: {sorted(set(expected) - actual)}; extra: {sorted(actual - set(expected))}")
    lines = []
    for name, goos in sorted(expected.items()):
        path = output / name
        verify_archive(path, goos)
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        lines.append(f"{digest}  {name}\n")
    return "".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("metadata")
    pack = sub.add_parser("package")
    pack.add_argument("--version", required=True)
    pack.add_argument("--goos", choices=EXTENSIONS, required=True)
    pack.add_argument("--goarch", choices=("amd64", "arm64"), required=True)
    pack.add_argument("--library", required=True)
    pack.add_argument("--output", default="dist")
    checksums = sub.add_parser("checksums")
    checksums.add_argument("--version", required=True)
    checksums.add_argument("--output", default="dist")
    args = parser.parse_args()
    if args.command == "metadata":
        metadata()
    elif args.command == "package":
        package(args.version, args.goos, args.goarch, args.library, args.output)
    else:
        content = checksum_content(args.version, args.output)
        (Path(args.output) / "checksums.txt").write_text(content, encoding="utf-8", newline="\n")
        print(content, end="")


if __name__ == "__main__":
    main()
