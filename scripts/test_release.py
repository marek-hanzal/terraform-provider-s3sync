"""Regression proofs for interrupted publication and immutable version bytes."""

import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import release


class ReleaseRecoveryTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.commit = "a" * 40
        self.output = self.root / "build/releases/0.1.0"
        self.output.mkdir(parents=True)
        (self.root / "docs").mkdir()
        (self.root / "docs/release-notes.md").write_text("Prototype evidence.\n")
        manifest = {"commit": self.commit, "version": "0.1.0", "sha256": {}}
        for platform in release.PLATFORMS:
            body = platform.encode()
            name = f"terraform-provider-s3sync_v0.1.0_{platform}"
            (self.output / name).write_bytes(body)
            manifest["sha256"][platform] = hashlib.sha256(body).hexdigest()
        (self.output / "provider-manifest.json").write_text(json.dumps(manifest))
        (self.output / "provenance.json").write_text("source provenance")
        (self.output / "SHA256SUMS").write_text("binary checksums")
        self.remote = None
        self.bodies = {}
        self.fail_upload = False
        self.uploaded = []
        for target, value in (("ROOT", self.root), ("source", lambda: self.commit),
                              ("version", lambda: "0.1.0"), ("ensure_tag", lambda *args: None),
                              ("run", self.command)):
            mock = patch.object(release, target, value)
            mock.start()
            self.addCleanup(mock.stop)

    def command(self, *args, **kwargs):
        if args[:2] == ("gh", "api"):
            return json.dumps([[self.remote] if self.remote else []])
        operation = args[2]
        if operation == "create":
            self.remote = {"tag_name": "v0.1.0", "draft": True,
                           "body": "Prototype evidence.", "assets": []}
        elif operation == "upload":
            paths = args[4:args.index("--repo")]
            for item in paths:
                path = Path(item)
                self.assertNotIn(path.name, self.bodies)
                self.bodies[path.name] = path.read_bytes()
                self.remote["assets"].append({"name": path.name})
                self.uploaded.append(path.name)
                if self.fail_upload:
                    self.fail_upload = False
                    raise subprocess.CalledProcessError(1, args)
        elif operation == "download":
            directory = Path(args[args.index("--dir") + 1])
            for name, body in self.bodies.items():
                (directory / name).write_bytes(body)
        elif operation == "edit":
            self.assertEqual(len(self.bodies), 6)
            self.remote["draft"] = False
        else:
            self.fail(f"unexpected command: {args}")

    def test_interrupted_upload_remains_draft_and_resumes_without_replacing_bytes(self):
        self.fail_upload = True
        with self.assertRaises(subprocess.CalledProcessError):
            release.publish()
        self.assertTrue(self.remote["draft"])
        accepted = dict(self.bodies)
        release.publish()
        self.assertFalse(self.remote["draft"])
        self.assertEqual(len(self.uploaded), 6)
        for name, body in accepted.items():
            self.assertEqual(self.bodies[name], body)
        release.publish()  # An exact published version is an idempotent no-op.
        self.assertEqual(len(self.uploaded), 6)

    def test_divergent_draft_or_published_artifact_is_never_replaced(self):
        release.publish()
        name = next(iter(self.bodies))
        self.bodies[name] = b"different remote bytes"
        for draft in (True, False):
            self.remote["draft"] = draft
            with self.assertRaisesRegex(SystemExit, "different bytes"):
                release.publish()
        self.assertEqual(len(self.uploaded), 6)


class TagRecoveryTest(unittest.TestCase):
    def test_existing_local_tag_is_reused_after_failed_release_creation(self):
        commit = "b" * 40
        with patch.object(release, "run", return_value="") as command, patch.object(
                release.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, commit + "\n")):
            release.ensure_tag("v0.1.0", commit, "0.1.0")
        self.assertFalse(any(call.args[:2] == ("git", "tag") for call in command.call_args_list))
        self.assertEqual(command.call_args_list[-1].args[:3], ("git", "push", "--atomic"))

    def test_existing_remote_tag_must_resolve_to_the_source_commit(self):
        with patch.object(release, "run", return_value="wrong\trefs/tags/v0.1.0\n") as command, patch.object(
                release.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "")):
            with self.assertRaisesRegex(SystemExit, "different source commit"):
                release.ensure_tag("v0.1.0", "b" * 40, "0.1.0")
        self.assertEqual(command.call_count, 1)


if __name__ == "__main__":
    unittest.main()
