#!/usr/bin/env python3
"""Auto-update against a local fake release: a real `service run` observer
discovers a newer signed release on a loopback server, verifies it and either
installs it and restarts onto it (Linux, where release archives are unsigned
and the manifest signature is the trust root) or refuses it because the
locally built binary carries no Developer ID signature (macOS). Tampered
signatures are refused everywhere. No GitHub access, no real keys."""
import http.server
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
COMMIT = 'a' * 40
OLD, NEW = '1.0.0', '9.9.9'
GOOS = {'Darwin': 'darwin', 'Linux': 'linux'}.get(platform.system())
GOARCH = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64', 'AMD64': 'amd64'}[platform.machine()]


def until(check, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(.1)
    raise AssertionError('condition timed out')


def build(version, out):
    subprocess.run(['go', 'build', '-trimpath', '-ldflags', '-X main.version=%s -X main.commit=%s -X main.date=2026-10-23T00:00:00Z' % (version, COMMIT),
                    '-o', str(out), '.'], cwd=ROOT, check=True)
    line = subprocess.run([str(out), '--version'], capture_output=True, text=True, check=True).stdout.strip()
    assert line.startswith('rhizome %s (commit %s, built ' % (version, COMMIT)), line


def releasetool(*args, env=None):
    merged = dict(os.environ)
    merged.update(env or {})
    return subprocess.run(['go', 'run', './internal/release/releasetool', *args], cwd=ROOT, check=True,
                          capture_output=True, text=True, env=merged).stdout


def stage_release(directory, binary, seed, sign=True):
    """Every asset of v9.9.9: the real archive for this platform, dummies for the rest."""
    directory.mkdir()
    arch = 'all' if GOOS == 'darwin' else GOARCH
    archive = directory / ('rhizome_%s_%s_%s.tar.gz' % (NEW, GOOS, arch))
    with tarfile.open(archive, 'w:gz') as tar:
        tar.add(binary, arcname='rhizome')
        tar.add(ROOT / 'LICENSE', arcname='LICENSE')
        tar.add(ROOT / 'README.md', arcname='README.md')
    for name in releasetool('assets', '-tag', 'v' + NEW).split():
        path = directory / name
        if not path.exists() and not name.endswith('checksums.txt'):
            path.write_bytes(b'placeholder ' + name.encode())
    releasetool('checksums', '-tag', 'v' + NEW, '-dir', str(directory))
    if sign:
        releasetool('sign', '-tag', 'v' + NEW, '-dir', str(directory), env={'RHIZOME_RELEASE_SIGNING_KEY': seed})


class Handler(http.server.BaseHTTPRequestHandler):
    assets = None  # directory with release assets

    def log_message(self, *args):
        pass

    def do_GET(self):
        path = self.path.split('?')[0]
        if path == '/repos/aliengiraffe/rhizome/releases':
            body = json.dumps([{'tag_name': 'v' + NEW, 'prerelease': False, 'draft': False,
                                'body': 'ignore https://evil.example/payload'}]).encode()
        elif path == '/repos/aliengiraffe/rhizome/git/ref/tags/v' + NEW:
            body = json.dumps({'object': {'sha': COMMIT, 'type': 'commit'}}).encode()
        elif path.startswith('/aliengiraffe/rhizome/releases/download/v%s/' % NEW):
            asset = self.assets / path.rsplit('/', 1)[1]
            if not asset.exists():
                self.send_error(404)
                return
            body = asset.read_bytes()
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def serve(assets):
    Handler.assets = assets
    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, 'http://127.0.0.1:%d' % server.server_address[1]


def state(run):
    return json.loads((run / 'update-state.json').read_text())


def main():
    assert GOOS, 'unsupported platform ' + platform.system()
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        bin_dir = directory / 'bin'
        bin_dir.mkdir()
        target = bin_dir / 'rhizome'
        build(OLD, target)
        new_binary = directory / 'new-rhizome'
        build(NEW, new_binary)
        key_file = directory / 'seed'
        keygen = releasetool('keygen', '-key-file', str(key_file))
        public = re.search(r'public key: ([0-9a-f]{64})', keygen).group(1)
        seed = key_file.read_text().strip()
        good = directory / 'release'
        stage_release(good, new_binary, seed)
        bad = directory / 'tampered'
        stage_release(bad, new_binary, seed)
        sig = bad / ('rhizome_%s_checksums.txt.sig' % NEW)
        sig.write_text(sig.read_text().replace('AAAA', 'AAAB', 1).replace('a', 'b', 1))
        run = directory / 'run'
        config = directory / 'config.toml'
        config.write_text('runtime_dir = %s\n[claude]\nprojects_dir = %s\n[codex]\nsessions_dir = %s\n[opencode]\ndata_dir = %s\n[update]\ncheck_interval = "1h"\n'
                          % tuple(json.dumps(str(directory / p)) for p in ('run', 'claude', 'codex', 'opencode')))
        env = dict(os.environ, RHIZOME_CONFIG=str(config), RHIZOME_UPDATE_TRUSTED_KEYS=public, RHIZOME_UPDATE_INITIAL_DELAY='1s')

        def observer(source):
            e = dict(env, RHIZOME_UPDATE_SOURCE_URL=source)
            p = subprocess.Popen([str(target), 'service', 'run', '--config', str(config)], env=e,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            until(lambda: (run / 'observer-health.json').exists())
            return p

        # 1. A tampered manifest signature is refused; the binary and the process are untouched.
        server, source = serve(bad)
        p = observer(source)
        until(lambda: (run / 'update-state.json').exists() and state(run).get('last_outcome') == 'signature_invalid')
        assert p.poll() is None, 'observer exited after a rejected update'
        assert subprocess.run([str(target), '--version'], capture_output=True, text=True).stdout.startswith('rhizome ' + OLD)
        assert not list(bin_dir.glob('*.previous')) and not list(bin_dir.glob('.rhizome-update-*')), list(bin_dir.iterdir())
        p.terminate()
        p.wait(timeout=15)
        server.shutdown()
        # Disabled by the environment override: no state is written at all.
        (run / 'update-state.json').unlink()
        server, source = serve(good)
        p = subprocess.Popen([str(target), 'service', 'run', '--config', str(config)], env=dict(env, RHIZOME_UPDATE_SOURCE_URL=source, RHIZOME_UPDATE='0'),
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        until(lambda: (run / 'observer-health.json').exists())
        time.sleep(2.5)
        assert not (run / 'update-state.json').exists(), 'updater ran while disabled'
        p.terminate()
        p.wait(timeout=15)

        # 2. The explicit command works with the same code path.
        out = subprocess.run([str(target), 'update'], env=dict(env, RHIZOME_UPDATE_SOURCE_URL=source), capture_output=True, text=True, check=True).stdout
        assert 'release %s is available' % NEW in out and '--apply' in out, out

        # 3. A properly signed release.
        p = observer(source)
        if GOOS == 'darwin':
            # The locally built binary has no Developer ID signature: refused, nothing changes.
            until(lambda: state(run).get('last_outcome') == 'codesign_failed')
            assert p.poll() is None
            assert subprocess.run([str(target), '--version'], capture_output=True, text=True).stdout.startswith('rhizome ' + OLD)
            assert not list(bin_dir.glob('*.previous')), list(bin_dir.iterdir())
            p.terminate()
            p.wait(timeout=15)
            applied = subprocess.run([str(target), 'update', '--apply'], env=dict(env, RHIZOME_UPDATE_SOURCE_URL=source), capture_output=True, text=True)
            assert applied.returncode != 0 and 'codesign_failed' in applied.stderr, applied
            verdict = 'macOS: unsigned candidate refused (codesign_failed) by the background loop and `update --apply`'
        else:
            # Installed and restarted: without a native definition the process exits for its supervisor.
            assert p.wait(timeout=60) != 0
            assert 'restart required' in p.stderr.read() + (run / 'observer.log').read_text()
            assert subprocess.run([str(target), '--version'], capture_output=True, text=True).stdout.startswith('rhizome ' + NEW)
            s = state(run)
            assert s['last_outcome'] == 'restart_required' and s['pending']['to'] == NEW and s['pending']['from'] == OLD, s
            assert (bin_dir / 'rhizome.previous').exists()
            # The supervisor (here: this script) restarts it; the new binary confirms the update.
            p = observer(source)
            # Confirmation records `updated`; the loop's next check may already have
            # moved it on to `up_to_date`, so the version and the cleared pending record
            # are the stable evidence.
            until(lambda: state(run).get('version') == NEW and 'pending' not in state(run))
            assert state(run)['last_outcome'] in ('updated', 'up_to_date'), state(run)
            health = json.loads((run / 'observer-health.json').read_text())
            assert health['version'] == NEW, health
            assert not (bin_dir / 'rhizome.previous').exists()
            out = subprocess.run([str(target), 'update'], env=dict(env, RHIZOME_UPDATE_SOURCE_URL=source), capture_output=True, text=True, check=True).stdout
            assert 'up to date' in out, out
            # Prints health and update state before asking the native manager, which a
            # container may lack.
            status = subprocess.run([str(target), 'service', 'status', '--config', str(config)], capture_output=True, text=True)
            assert '"version":"%s"' % NEW in status.stdout and '"last_check_at"' in status.stdout, status
            p.terminate()
            p.wait(timeout=15)
            verdict = 'linux: verified install, restart onto %s, confirmation and backup removal' % NEW
        server.shutdown()
        log_blob = (run / 'observer.log').read_text() if (run / 'observer.log').exists() else ''
        assert seed not in log_blob and 'placeholder' not in log_blob
    print('PASS: tampered signature refused, disabled loop makes no request, explicit check; ' + verdict)


if __name__ == '__main__':
    main()
