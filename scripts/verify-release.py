#!/usr/bin/env python3
"""Verify the six release archives, embedded versions, and Linux helper."""
import hashlib
from pathlib import Path
import platform
import subprocess
import sys
import tarfile
import tempfile
import zipfile


def verify(directory, tag, commit):
    version = tag.removeprefix('v')
    expected = {
        f'swarm_{version}_{system}_{arch}.{extension}': (system, arch)
        for system, extension in [('linux', 'tar.gz'), ('darwin', 'tar.gz'), ('windows', 'zip')]
        for arch in ['amd64', 'arm64']
    }
    checksums = {}
    for line in (directory / f'swarm_{version}_checksums.txt').read_text().splitlines():
        digest, name = line.split()
        assert name not in checksums, f'Duplicate checksum: {name}'
        checksums[name] = digest
    assert checksums.keys() == expected.keys(), f'Unexpected archive set: {checksums.keys()}'
    for name, (system, arch) in expected.items():
        path = directory / name
        assert hashlib.sha256(path.read_bytes()).hexdigest() == checksums[name], f'Checksum mismatch: {name}'
        if system == 'windows':
            with zipfile.ZipFile(path) as archive:
                files = {name: archive.read(name) for name in archive.namelist() if not name.endswith('/')}
        else:
            with tarfile.open(path) as archive:
                files = {item.name: archive.extractfile(item).read() for item in archive if item.isfile()}
        assert 'README.md' in files and 'LICENSE' in files, f'Missing documentation: {name}'
        binary = 'swarm.exe' if system == 'windows' else 'swarm'
        assert binary in files, f'Missing CLI: {name}'
        assert ('swarm-restore-helper' in files) == (system == 'linux'), f'Incorrect helper packaging: {name}'
        with tempfile.TemporaryDirectory() as temporary:
            executable = Path(temporary, binary)
            executable.write_bytes(files[binary])
            metadata = subprocess.check_output(['go', 'version', '-m', str(executable)], text=True)
            for setting in [f'GOOS={system}', f'GOARCH={arch}', 'CGO_ENABLED=0',
                            f'/internal/cli.Version={version}', f'/internal/cli.Commit={commit}']:
                assert setting in metadata, f'Missing {setting}: {name}'
            if system == 'linux':
                helper = Path(temporary, 'swarm-restore-helper')
                helper.write_bytes(files[helper.name])
                info = subprocess.check_output(['go', 'version', '-m', str(helper)], text=True)
                for setting in ['/cmd/swarm-restore-helper', 'CGO_ENABLED=0', 'GOOS=linux', f'GOARCH={arch}']:
                    assert setting in info, f'Invalid helper {setting}: {name}'
            if system == platform.system().lower() and arch == {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine()):
                executable.chmod(0o755)
                output = subprocess.check_output([str(executable), 'version'], text=True, timeout=10)
                assert f'swarm version {version} ' in output and commit in output, output
                print(output.strip())
        print(f'Verified {name}')


if __name__ == '__main__':
    verify(Path(sys.argv[1]), sys.argv[2], sys.argv[3])
