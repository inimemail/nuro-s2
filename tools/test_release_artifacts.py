import tempfile
import unittest
import json
import subprocess
import sys
from pathlib import Path
from release_artifacts import LOCKS, manifest, release_inventory, resolve_identity


class ReleaseArtifactsTests(unittest.TestCase):
    def test_inventory_cli_records_all_component_identities_and_toolchains(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in LOCKS:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('locked')
            artifact = root / 'artifact'
            artifact.mkdir()
            (artifact / 'binary').write_bytes(b'fixture-artifact')
            sha, tag = 'a' * 40, 'v0.2.19-nuro.1'
            component_args = []
            for name, tools in [('frontend', {'node': 'v24.13.0', 'pnpm': '9.15.9'}), ('version', None)]:
                path = root / (name + '-manifest.json')
                path.write_text(json.dumps(manifest(root, artifact, sha, tag, tools)))
                component_args.extend(['--component-manifest', str(path)])
            output = root / 'release-manifest.json'
            result = subprocess.run([sys.executable, str(Path(__file__).with_name('release_artifacts.py')),
                'inventory', '--root', str(root), '--artifact', str(artifact), '--manifest', str(output),
                '--sha', sha, '--tag', tag, '--image-digest', 'sha256:' + 'c' * 64,
                '--toolchain', 'go=go1.27.3', '--toolchain', 'rust=1.90.0', *component_args],
                capture_output=True, text=True)
            self.assertEqual(0, result.returncode, result.stderr)
            written = json.loads(output.read_text())
            self.assertEqual(sha, written['sha'])
            self.assertEqual({'go': 'go1.27.3', 'rust': '1.90.0', 'node': 'v24.13.0', 'pnpm': '9.15.9'}, written['toolchains'])
            self.assertEqual({'frontend', 'version'}, set(written['components']))

    def test_tag_source_wins_over_dispatch_branch_and_versions_must_match(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def git(*args):
                return subprocess.check_output(['git', *args], cwd=root, text=True, stderr=subprocess.DEVNULL).strip()
            git('init', '-q')
            git('config', 'user.name', 'Release Fixture')
            git('config', 'user.email', 'fixture@example.test')
            frontend = root / 'frontend/package.json'
            backend = root / 'backend/cmd/server/VERSION'
            frontend.parent.mkdir(parents=True)
            backend.parent.mkdir(parents=True)
            frontend.write_text(json.dumps({'version': '0.2.19-nuro.1'}))
            backend.write_text('0.2.19-nuro.1\n')
            git('add', '.')
            git('commit', '-qm', 'Release fixture')
            expected = git('rev-parse', 'HEAD')
            git('tag', 'v0.2.19-nuro.1')
            frontend.write_text(json.dumps({'version': '0.2.20-nuro.1'}))
            git('add', '.')
            git('commit', '-qm', 'Different dispatch branch')
            self.assertNotEqual(expected, git('rev-parse', 'HEAD'))
            self.assertEqual(expected, resolve_identity(root, 'v0.2.19-nuro.1'))
            git('tag', 'v0.2.20-nuro.1')
            with self.assertRaisesRegex(ValueError, 'versions must match'):
                resolve_identity(root, 'v0.2.20-nuro.1')
            for tag in ['', 'v1;false', 'v0.2.19\n', '--help']:
                with self.assertRaises(ValueError):
                    resolve_identity(root, tag)
            with self.assertRaises(subprocess.CalledProcessError):
                resolve_identity(root, 'v9.9.9')

    def test_identity_content_and_locks_are_all_bound(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in LOCKS:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('locked')
            artifact = root / 'artifact'
            artifact.mkdir()
            (artifact / 'index.html').write_text('built')
            first = manifest(root, artifact, 'a' * 40, 'v0.2.19-nuro.1')
            toolchains = {'go': 'go1.27.3', 'rust': '1.90.0', 'node': 'v24.13.0', 'pnpm': '9.15.9'}
            components = {'frontend': {**first, 'toolchains': {k: toolchains[k] for k in ('node', 'pnpm')}}, 'version': first}
            inventory = release_inventory(root, artifact, 'a' * 40, 'v0.2.19-nuro.1', components, toolchains, 'sha256:' + 'c' * 64)
            self.assertEqual(toolchains, inventory['toolchains'])
            self.assertEqual(first['files'], inventory['files'])
            with self.assertRaisesRegex(ValueError, 'source identity'):
                release_inventory(root, artifact, 'b' * 40, 'v0.2.19-nuro.1', components, toolchains, 'sha256:' + 'c' * 64)
            with self.assertRaisesRegex(ValueError, 'toolchains'):
                release_inventory(root, artifact, 'a' * 40, 'v0.2.19-nuro.1', components, {}, 'sha256:' + 'c' * 64)
            with self.assertRaisesRegex(ValueError, 'image digest'):
                release_inventory(root, artifact, 'a' * 40, 'v0.2.19-nuro.1', components, toolchains, '')
            self.assertNotEqual(first, manifest(root, artifact, 'b' * 40, 'v0.2.19-nuro.1'))
            (artifact / 'index.html').write_text('tampered')
            self.assertNotEqual(first, manifest(root, artifact, 'a' * 40, 'v0.2.19-nuro.1'))
            (artifact / 'index.html').write_text('built')
            (root / LOCKS[0]).write_text('changed')
            self.assertNotEqual(first, manifest(root, artifact, 'a' * 40, 'v0.2.19-nuro.1'))
            with self.assertRaises(ValueError):
                manifest(root, artifact, 'a' * 40, 'v1;false')
            (artifact / 'link').symlink_to(root / LOCKS[0])
            with self.assertRaises(ValueError):
                manifest(root, artifact, 'a' * 40, 'v0.2.19')


if __name__ == '__main__':
    unittest.main()
