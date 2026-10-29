#!/usr/bin/env python3
"""Bind prebuilt artifacts to the resolved release commit and dependency locks."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

LOCKS = ('backend/go.mod', 'backend/go.sum', 'frontend/package.json', 'frontend/pnpm-lock.yaml', 'edge-rs/Cargo.lock')
TAG_PATTERN = r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?'


def resolve_identity(root, tag):
    if not re.fullmatch(TAG_PATTERN, tag):
        raise ValueError('Invalid release tag')
    sha = subprocess.check_output(
        ['git', 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}'], cwd=root, text=True, stderr=subprocess.PIPE).strip()
    if not re.fullmatch(r'[0-9a-f]{40}', sha):
        raise ValueError('Invalid release commit')
    # Inspect the selected commit, never the dispatch branch's worktree.
    def source_file(name):
        return subprocess.check_output(['git', 'show', f'{sha}:{name}'], cwd=root, text=True)
    frontend_version = json.loads(source_file('frontend/package.json'))['version']
    backend_version = source_file('backend/cmd/server/VERSION').strip().removeprefix('v')
    if frontend_version != tag[1:] or backend_version != tag[1:]:
        raise ValueError('Release tag, frontend and backend versions must match')
    return sha


def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def manifest(root, artifact, sha, tag, toolchains=None):
    if not re.fullmatch(r'[0-9a-f]{40}', sha) or not re.fullmatch(TAG_PATTERN, tag):
        raise ValueError('Invalid release identity')
    files = {}
    for path in sorted(artifact.rglob('*')):
        if path.is_symlink():
            raise ValueError('Artifact symlinks are not supported')
        if path.is_file():
            files[path.relative_to(artifact).as_posix()] = digest(path)
    if not files:
        raise ValueError('Empty artifact')
    result = {'sha': sha, 'tag': tag, 'locks': {p: digest(root / p) for p in LOCKS}, 'files': files}
    if toolchains:
        result['toolchains'] = toolchains
    return result


def release_inventory(root, artifact, sha, tag, components, toolchains, image_digest):
    result = manifest(root, artifact, sha, tag)
    if set(toolchains) != {'go', 'rust', 'node', 'pnpm'} or any(not v for v in toolchains.values()):
        raise ValueError('All component toolchains must be recorded')
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', image_digest):
        raise ValueError('A resolved container image digest is required')
    if set(components) != {'frontend', 'version'}:
        raise ValueError('Frontend and version component manifests are required')
    for component in components.values():
        if any(component.get(field) != result[field] for field in ('sha', 'tag', 'locks')):
            raise ValueError('Component source identity or dependency locks do not match')
    if components['frontend'].get('toolchains') != {k: toolchains[k] for k in ('node', 'pnpm')}:
        raise ValueError('Frontend build toolchains must match its component manifest')
    result.update({'components': components, 'toolchains': toolchains, 'ghcr_image_digest': image_digest})
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('mode', choices=['resolve', 'write', 'verify', 'inventory'])
    parser.add_argument('--root', type=Path, default=Path('.'))
    parser.add_argument('--artifact', type=Path)
    parser.add_argument('--manifest', type=Path)
    parser.add_argument('--sha')
    parser.add_argument('--tag', required=True)
    parser.add_argument('--component-manifest', type=Path, action='append', default=[])
    parser.add_argument('--toolchain', action='append', default=[])
    parser.add_argument('--image-digest')
    args = parser.parse_args()
    if args.mode == 'resolve':
        print(resolve_identity(args.root, args.tag))
        return
    if args.artifact is None or args.manifest is None or args.sha is None:
        parser.error('write and verify require --artifact, --manifest and --sha')
    toolchains = dict(value.split('=', 1) for value in args.toolchain)
    if args.mode == 'inventory':
        components = {p.stem.removesuffix('-manifest'): json.loads(p.read_text()) for p in args.component_manifest}
        toolchains.update(components.get('frontend', {}).get('toolchains', {}))
        expected = release_inventory(args.root, args.artifact, args.sha, args.tag, components, toolchains, args.image_digest or '')
    else:
        expected = manifest(args.root, args.artifact, args.sha, args.tag, toolchains)
    if args.mode in ('write', 'inventory'):
        args.manifest.write_text(json.dumps(expected, indent=2) + '\n')
    elif json.loads(args.manifest.read_text()) != expected:
        raise SystemExit('Artifact identity, content or dependency locks do not match')


if __name__ == '__main__':
    main()
