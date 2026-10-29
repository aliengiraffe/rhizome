#!/usr/bin/env python3
"""Live Pi binding check: a real `pi --mode rpc` loads the embedded extension, which
registers `rhizome mcp --agent pi`; the Go side must bind exactly the Pi session and
re-bind on /new. No model turn and no credentials are used. Skips without `pi`.

Run after `task build`: python3 scripts/smoke_pi.py [--bin ./rhizome]
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
binary = Path(sys.argv[sys.argv.index('--bin') + 1] if '--bin' in sys.argv else ROOT / 'rhizome').resolve()


def fail(message):
    print('FAIL:', message)
    try:
        print(open(os.path.join(root, 'pi.err')).read()[-1500:])
        print(open(os.path.join(root, 'rhizome.log')).read()[-1500:])
    except Exception:
        pass
    sys.exit(1)


if shutil.which('pi') is None:
    print('SKIP: pi is not installed')
    sys.exit(0)
if not binary.exists():
    fail(f'{binary} not found; run task build')

root = Path(tempfile.mkdtemp(prefix='rzpi-'))  # short path: Unix socket names are bounded
try:
    agent, work, run = (root / d for d in ('agent', 'work', 'run'))
    for d in (agent / 'extensions' / 'rhizome', work):
        d.mkdir(parents=True)
    run.mkdir(mode=0o700)
    shutil.copy(ROOT / 'plugins' / 'pi' / 'rhizome.js', agent / 'extensions' / 'rhizome' / 'index.js')
    env = {k: v for k, v in os.environ.items() if not k.startswith(('RHIZOME_', 'PI_', 'XDG_'))}
    # HOME stays real: version-manager shims for `pi` need it. Rhizome's own state is
    # isolated through an empty config file and a private runtime directory instead.
    (root / 'config.toml').write_text('')
    log = root / 'rhizome.log'
    wrapper = root / 'rhizome-wrapper'  # Pi discards child stderr; keep it for the check
    wrapper.write_text(f'#!/bin/sh\nexec "{binary}" "$@" 2>>"{log}"\n')
    wrapper.chmod(0o755)
    env.update(RHIZOME_CONFIG=str(root / 'config.toml'), PI_CODING_AGENT_DIR=str(agent), RHIZOME_BIN=str(wrapper), RHIZOME_RUNTIME_DIR=str(run))
    proc = subprocess.Popen(['pi', '--mode', 'rpc', '--no-session'], cwd=work, env=env, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=open(root / 'pi.err', 'w'), text=True)
    responses = {}

    def drain():  # an undrained stdout pipe blocks pi
        for line in proc.stdout:
            try:
                o = json.loads(line)
            except ValueError:
                continue
            if o.get('type') == 'response':
                responses[o.get('id')] = o

    threading.Thread(target=drain, daemon=True).start()

    def call(i, type_):
        # Pi reads stdin only after startup; resend until the first answer arrives.
        deadline = time.time() + 30
        sent = 0.0
        while i not in responses:
            if time.time() > deadline:
                fail(f'no response to {type_}')
            if time.time() - sent > 2:
                proc.stdin.write(json.dumps({'id': i, 'type': type_}) + '\n')
                proc.stdin.flush()
                sent = time.time()
            time.sleep(0.05)
        return responses[i]['data']

    def bindings():
        """Session IDs the Go side logged as bound (metadata only), in order."""
        out = []
        try:
            lines = log.read_text().splitlines()
        except OSError:
            return out
        for line in lines:
            try:
                r = json.loads(line)
            except ValueError:
                continue
            if r.get('msg') == 'binding' and r.get('agent') == 'pi':
                out.append(r.get('session_id'))
            elif r.get('level') in ('WARN', 'ERROR') and str(r.get('msg', '')).startswith('pi_'):
                fail(f'unexpected warning from rhizome: {line}')
        return out

    def wait_for(session_id, count):
        deadline = time.time() + 20
        while time.time() < deadline:
            if len(bindings()) >= count:
                got = bindings()
                if got[count - 1] != session_id:
                    fail(f'binding {count} is {got[count - 1]}, want {session_id}')
                return
            time.sleep(0.1)
        fail(f'session {session_id} never bound; bindings={bindings()}')

    first = call('1', 'get_state')['sessionId']
    wait_for(first, 1)
    print('bound first session ...' + first[-8:])
    call('2', 'new_session')
    second = call('3', 'get_state')['sessionId']
    if second == first:
        fail('new_session did not change the session ID')
    wait_for(second, 2)
    print('rebound after /new ...' + second[-8:])
    time.sleep(1)  # a duplicate session_start must not cause a third bind
    if bindings() != [first, second]:
        fail(f'unexpected bindings {bindings()}')
    proc.stdin.close()
    proc.wait(timeout=15)
    print('OK: pi sessions bind once per session ID and rebind on /new')
finally:
    shutil.rmtree(root, ignore_errors=True)
