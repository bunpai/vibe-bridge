import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import artifacts
import draft_release


class ArtifactTests(unittest.TestCase):
    def test_metadata_covers_the_required_six_platforms(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "github-output"
            with patch.dict(os.environ, GITHUB_REF_TYPE="branch", GITHUB_REF_NAME="main",
                            GITHUB_SHA="a" * 40, GITHUB_OUTPUT=str(output)):
                artifacts.metadata()
            values = dict(line.split("=", 1) for line in output.read_text(encoding="utf-8").splitlines())
            matrix = json.loads(values["matrix"])["include"]
            platforms = {(entry["goos"], entry["goarch"]): entry for entry in matrix}
            self.assertEqual(set(platforms), {
                ("linux", "amd64"), ("linux", "arm64"),
                ("darwin", "amd64"), ("darwin", "arm64"),
                ("windows", "amd64"), ("windows", "arm64"),
            })
            self.assertEqual(len(matrix), 6)
            windows_arm64 = platforms["windows", "arm64"]
            self.assertEqual(windows_arm64["runner"], "windows-11-arm")
            self.assertEqual(windows_arm64["cc"], "clang")
            self.assertEqual(windows_arm64["library"], "vibe-bridge.dll")
            self.assertEqual(windows_arm64["cflags"], "-O2 -g -march=armv8-a")

    def test_versions_for_main_and_numeric_tags(self):
        self.assertEqual(artifacts.build_version("branch", "main", "a" * 40), "dev-aaaaaaaaaaaa")
        self.assertEqual(artifacts.build_version("tag", "v1.2.3", "a" * 40), "1.2.3")
        self.assertEqual(artifacts.release_version("v1.2.3.4"), "1.2.3.4")
        for tag in ("1.2.3", "v1", "v1.2.3-rc1", "v1.2.3+meta", "v1.2.3\n", "v1.2/3"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                artifacts.release_version(tag)
        with self.assertRaises(ValueError):
            artifacts.build_version("branch", "feature", "a" * 40)

    def test_zip_layout_and_checksums_for_all_platforms(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            output = root / "dist"
            for goos, goarch, *_ in artifacts.PLATFORMS:
                library = root / artifacts.library_name(goos)
                library.write_bytes(f"test binary {goos}/{goarch}".encode())
                archive = artifacts.package("1.2.3", goos, goarch, library, output)
                with zipfile.ZipFile(archive) as packaged:
                    self.assertEqual(packaged.namelist(), [library.name])
                    self.assertEqual(packaged.read(library.name), library.read_bytes())
                original = archive.read_bytes()
                os.utime(library, (1800000000, 1800000000))
                artifacts.package("1.2.3", goos, goarch, library, output)
                self.assertEqual(archive.read_bytes(), original)
            checksums = artifacts.checksum_content("1.2.3", output)
            self.assertEqual(len(checksums.splitlines()), len(artifacts.PLATFORMS))
            for line in checksums.splitlines():
                digest, name = line.split("  ")
                self.assertEqual(digest, hashlib.sha256((output / name).read_bytes()).hexdigest())
            (output / "checksums.txt").write_text(checksums, encoding="utf-8")
            self.assertEqual(artifacts.checksum_content("1.2.3", output), checksums)
            (output / "vibe-bridge_1.2.3_windows_arm64.zip").unlink()
            with self.assertRaisesRegex(ValueError, "Missing:.*vibe-bridge_1.2.3_windows_arm64.zip"):
                artifacts.checksum_content("1.2.3", output)

    def test_invalid_zip_layouts_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "test.zip"
            for names in (("nested/vibe-bridge.so",), ("../vibe-bridge.so",),
                          ("/vibe-bridge.so",), ("other.so",),
                          ("vibe-bridge.so", "extra.so"), ("vibe-bridge.so", "vibe-bridge.h")):
                with self.subTest(names=names):
                    with zipfile.ZipFile(archive, "w") as packaged:
                        for name in names:
                            packaged.writestr(name, b"binary")
                    with self.assertRaises(ValueError):
                        artifacts.verify_archive(archive, "linux")
            link = zipfile.ZipInfo("vibe-bridge.so")
            link.create_system = 3
            link.external_attr = 0o120777 << 16
            with zipfile.ZipFile(archive, "w") as packaged:
                packaged.writestr(link, "target")
            with self.assertRaises(ValueError):
                artifacts.verify_archive(archive, "linux")


class DraftTests(unittest.TestCase):
    def test_create_and_rerun_keep_release_draft(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, GH_REPO="owner/repo"):
            asset = Path(directory) / "checksums.txt"
            asset.write_text("test", encoding="utf-8")
            result = {"isDraft": True, "url": "https://github.com/owner/repo/releases/1",
                      "assets": [{"name": asset.name, "size": asset.stat().st_size}]}
            for existing in ([], [{"tag_name": "v1.2.3", "draft": True}]):
                with self.subTest(existing=existing), patch.object(draft_release, "gh") as gh:
                    gh.side_effect = [json.dumps([existing]), "", json.dumps(result)]
                    self.assertEqual(draft_release.ensure_draft("v1.2.3", "a" * 40, [str(asset)]), result["url"])
                    command = gh.call_args_list[1].args
                    if existing:
                        self.assertEqual(command[:3], ("release", "upload", "v1.2.3"))
                        self.assertIn("--clobber", command)
                    else:
                        self.assertEqual(command[:3], ("release", "create", "v1.2.3"))
                        self.assertIn("--draft", command)
                        self.assertIn("--verify-tag", command)

    def test_published_release_is_preserved(self):
        with patch.dict(os.environ, GH_REPO="owner/repo"), patch.object(draft_release, "gh") as gh:
            gh.return_value = json.dumps([[{"tag_name": "v1.2.3", "draft": False}]])
            with self.assertRaisesRegex(RuntimeError, "already published"):
                draft_release.ensure_draft("v1.2.3", "a" * 40, [])
            self.assertEqual(gh.call_count, 1)

    def test_missing_upload_fails_verification(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, GH_REPO="owner/repo"):
            asset = Path(directory) / "checksums.txt"
            asset.write_text("test", encoding="utf-8")
            with patch.object(draft_release, "gh") as gh:
                gh.side_effect = ["[[]]", "", json.dumps({"isDraft": True, "assets": []})]
                with self.assertRaisesRegex(RuntimeError, "verification"):
                    draft_release.ensure_draft("v1.2.3", "a" * 40, [str(asset)])


if __name__ == "__main__":
    unittest.main()
