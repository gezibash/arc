#!/usr/bin/env python3
"""Deploy the current source snapshot to the dedicated lab, using its temporary key."""
import argparse
import concurrent.futures
import hashlib
import json
import os
import shlex
import subprocess
import tarfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


class Remote:
    def __init__(self, state, key, output):
        if state.get('torn_down_at'):
            raise ValueError('This lab has already been torn down; create a fresh run.')
        addresses = state.get('resources', {}).get('public_addresses', {})
        if not all(addresses.get(role) for role in ('relay', 'citizen', 'provider')):
            raise ValueError('Lab creation is incomplete; deployment requires all three hosts.')
        self.state = state
        self.output = Path(output).resolve()
        self.output.mkdir(parents=True, exist_ok=True)
        controls = Path('/tmp') / ('arc-lab-ssh-' + str(os.getpid()))
        controls.mkdir(mode=0o700, exist_ok=False)
        self.base = ['ssh', '-i', str(Path(key).resolve()), '-o', 'IdentitiesOnly=yes',
                     '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=8',
                     '-o', 'ControlMaster=auto', '-o', 'ControlPersist=45',
                     '-o', 'ControlPath=' + str(controls / '%C'),
                     '-o', 'ServerAliveInterval=15', '-o', 'ServerAliveCountMax=3',
                     '-o', 'StrictHostKeyChecking=accept-new',
                     '-o', 'UserKnownHostsFile=' + str(self.output / 'known_hosts')]

    def command(self, role, command):
        address = self.state['resources']['public_addresses'][role]
        return self.base + ['ubuntu@' + address, command]

    def run(self, role, command, data=None, timeout=60):
        result = subprocess.run(self.command(role, command), input=data, capture_output=True, timeout=timeout)
        if result.returncode:
            raise RuntimeError(f'{role}: {result.stderr.decode(errors="replace")} {result.stdout.decode(errors="replace")}')
        return result.stdout.decode()

    def json_file(self, role, path, obj):
        self.run(role, 'umask 077; mkdir -p ' + shlex.quote(str(Path(path).parent)) + '; cat > ' + shlex.quote(path),
                 (json.dumps(obj) + '\n').encode())


def snapshot(destination):
    files = subprocess.check_output(['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'], cwd=ROOT).decode().split('\0')
    prefixes = ('apps/', 'config/', 'providers/sqlite/', 'test/fixtures/', 'scripts/traversal_lab/')
    exact = {'mix.exs', 'mix.lock', 'mise.toml'}
    selected = sorted({f for f in files if (f.startswith(prefixes) or f in exact)
                       and '__pycache__' not in f and not f.endswith('.pyc') and (ROOT / f).is_file()})
    hashes = {f: hashlib.sha256((ROOT / f).read_bytes()).hexdigest() for f in selected}
    with tarfile.open(destination, 'w:gz') as archive:
        for f in selected:
            if (ROOT / f).is_symlink():
                raise RuntimeError('Unexpected source symlink: ' + f)
            archive.add(ROOT / f, arcname=f, recursive=False)
    manifest = {'git_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT).decode().strip(),
                'sha256': hashlib.sha256(destination.read_bytes()).hexdigest(), 'files': hashes}
    destination.with_suffix('.manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    return manifest


INSTALL = r'''set -eu
export PATH=/home/ubuntu/.local/bin:$PATH
export MISE_ERLANG_COMPILE=false
export MISE_ERLANG_PRECOMPILED_OS=ubuntu-24.04
export MIX_ENV=prod
cd /opt/arc
mise trust /opt/arc/mise.toml
mise install
mise exec -- mix local.hex --force
mise exec -- mix local.rebar --force
mise exec -- mix deps.get --only prod
mise exec -- mix compile --warnings-as-errors
cd providers/sqlite
mise trust /opt/arc/providers/sqlite/mise.toml
mise install
mise exec -- python --version
cd /opt/arc
mise exec -- elixir --version
uname -sr
sudo systemctl is-active arc-lab-terminate.timer
'''


def deploy(remote, role, archive):
    deadline = time.monotonic() + 360
    while True:
        try:
            remote.run(role, 'test -x /home/ubuntu/.local/bin/mise && test -d /opt/arc && sudo cloud-init status --wait', timeout=25)
            break
        except (RuntimeError, subprocess.TimeoutExpired):
            if time.monotonic() > deadline:
                raise
            time.sleep(5)
    remote.run(role, 'tar xzf - -C /opt/arc', archive.read_bytes())
    log = remote.output / (role + '-install.log')
    with log.open('wb') as output:
        result = subprocess.run(remote.command(role, 'bash -s'), input=INSTALL.encode(), stdout=output, stderr=subprocess.STDOUT, timeout=900)
    if result.returncode:
        raise RuntimeError(f'{role} installation failed; see {log}')
    print(role + ': source deployed and runtime built', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--key', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    state = json.loads(args.state.read_text())
    remote = Remote(state, args.key, args.output)
    archive = remote.output / 'source.tar.gz'
    manifest = snapshot(archive)
    print('Source SHA256: ' + manifest['sha256'], flush=True)
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        futures = [pool.submit(deploy, remote, role, archive) for role in ('relay', 'citizen', 'provider')]
        for future in concurrent.futures.as_completed(futures):
            future.result()


if __name__ == '__main__':
    main()
