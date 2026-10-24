import hashlib
from pathlib import Path
import tempfile
import unittest
from check_upgrade_contracts import check_protected, check_migrations


class UpgradeFenceTests(unittest.TestCase):
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
