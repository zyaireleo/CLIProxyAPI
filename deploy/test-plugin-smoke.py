#!/usr/bin/env python3
"""Offline regression tests for the candidate smoke-test harness."""
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest
import unittest.mock
import zipfile

spec = importlib.util.spec_from_file_location("plugin_smoke", pathlib.Path(__file__).with_name("plugin-smoke.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class PluginSmokeTests(unittest.TestCase):
    def test_plugin_config_stays_block_yaml(self):
        raw = smoke.block_yaml({"plugins": {"configs": {"antigravity-priority": {"enabled": True}}},
                                "auth-dir": "/tmp/path with spaces", "api-keys": ["test-only"]})
        self.assertIn('      "enabled": true\n', raw)
        self.assertNotIn("{", raw)
        self.assertIn('"auth-dir": "/tmp/path with spaces"', raw)
        self.assertIn('"api-keys": ["test-only"]', raw)

    def test_network_namespace_is_required(self):
        with unittest.mock.patch.object(smoke.sys, "platform", "linux"), \
             unittest.mock.patch.object(smoke.socket, "if_nameindex", return_value=[(1, "lo"), (2, "eth0")]):
            with self.assertRaisesRegex(RuntimeError, "only loopback"):
                smoke.verify_isolation()

    def test_non_linux_is_rejected(self):
        with unittest.mock.patch.object(smoke.sys, "platform", "win32"):
            with self.assertRaisesRegex(RuntimeError, "requires Linux"):
                smoke.verify_isolation()

    def test_pinned_archives_extract_only_verified_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            archive = root / "fixture.zip"
            content = b"offline-shared-library-fixture"
            with zipfile.ZipFile(archive, "w") as package:
                package.writestr("../../unsafe/fixture.so", content)
            manifest = root / "manifest.json"
            manifest.write_text(json.dumps({"plugins": [{
                "id": "fixture", "filename": "fixture-v1.so",
                "sha256": hashlib.sha256(content).hexdigest(),
                "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
            }]}))
            with unittest.mock.patch.object(smoke, "MANIFEST", manifest):
                output = root / "plugins"
                smoke.extract_plugins(root, output)
                self.assertEqual((output / "fixture-v1.so").read_bytes(), content)
                self.assertEqual(list(output.iterdir()), [output / "fixture-v1.so"])
                (output / "fixture-v1.so").write_bytes(b"changed")
                with self.assertRaisesRegex(RuntimeError, "Existing plugin hash mismatch"):
                    smoke.extract_plugins(root, output)

    def test_wrong_archive_hash_is_rejected_before_extraction(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "fixture.zip").write_bytes(b"untrusted")
            manifest = root / "manifest.json"
            manifest.write_text(json.dumps({"plugins": [{
                "id": "fixture", "filename": "fixture-v1.so",
                "sha256": "0" * 64, "archive_sha256": "0" * 64,
            }]}))
            with unittest.mock.patch.object(smoke, "MANIFEST", manifest):
                with self.assertRaisesRegex(RuntimeError, "archive hash mismatch"):
                    smoke.extract_plugins(root, root / "plugins")
            self.assertEqual(list((root / "plugins").iterdir()), [])


if __name__ == "__main__":
    unittest.main()
