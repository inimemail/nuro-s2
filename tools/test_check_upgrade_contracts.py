import hashlib
from pathlib import Path
import tempfile
import unittest
from check_upgrade_contracts import check_protected, check_migrations, check_historical_migrations, validate_manifest, check_go_images


class UpgradeFenceTests(unittest.TestCase):
    def test_symlink_cannot_replace_protected_file_or_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "real").write_bytes(b"original")
            (root / "link").symlink_to(root / "real")
            manifest = {"files": {"link": hashlib.sha256(b"original").hexdigest()}}
            self.assertEqual(1, len(check_protected(root, manifest)))

    def test_historical_sql_content_and_deletion(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            sql = root / "backend/migrations/001_original.sql"
            sql.parent.mkdir(parents=True)
            sql.write_bytes(b"SELECT 1;")
            manifest = {"migrations": {sql.name: hashlib.sha256(sql.read_bytes()).hexdigest()}}
            self.assertEqual([], check_historical_migrations(root, manifest))
            sql.write_bytes(b"SELECT 2;")
            self.assertEqual(1, len(check_historical_migrations(root, manifest)))
            sql.unlink()
            self.assertEqual(1, len(check_historical_migrations(root, manifest)))

    def test_manifest_rejects_empty_or_malformed_hashes(self):
        for manifest in [{}, {"baseline": "a" * 40, "files": {}}, {"baseline": "a" * 40, "files": {"x": "bad"}}]:
            with self.assertRaises(ValueError):
                validate_manifest(manifest)

    def test_both_go_images_must_match(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "backend").mkdir()
            (root / "backend/go.mod").write_text("module test\ngo 1.27.3\n")
            (root / "Dockerfile").write_text("ARG GOLANG_IMAGE=golang:1.27.3-alpine\n")
            (root / "backend/Dockerfile").write_text("FROM golang:1.27.3-alpine\n")
            self.assertEqual([], check_go_images(root))
            (root / "backend/Dockerfile").write_text("FROM golang:1.26.1-alpine\n")
            self.assertEqual(1, len(check_go_images(root)))

    def test_changed_missing_and_unsafe_files_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "protected.go").write_bytes(b"original")
            manifest = {"files": {"protected.go": hashlib.sha256(b"original").hexdigest()}}
            self.assertEqual([], check_protected(root, manifest))
            (root / "protected.go").write_bytes(b"changed")
            self.assertEqual(1, len(check_protected(root, manifest)))
            (root / "protected.go").unlink()
            self.assertEqual(1, len(check_protected(root, manifest)))
            self.assertEqual(1, len(check_protected(root, {"files": {"../outside": "x"}})))

    def test_migration_collisions_normalize_numeric_ids(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "001_first.sql").touch()
            self.assertEqual([], check_migrations(root))
            (root / "1_collision.sql").touch()
            self.assertEqual(1, len(check_migrations(root)))
            self.assertEqual([], check_migrations(root, ["001_first.sql", "1_collision.sql"]))


if __name__ == "__main__":
    unittest.main()
