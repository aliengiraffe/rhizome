#!/usr/bin/env python3
"""Live Pi check: `rhizome setup --agents pi` installs the embedded extension and a real
`pi --mode rpc` loads it. Rhizome must bind exactly the Pi session (and rebind on /new),
publish the session's activity to a peer, and deliver a peer's `send` into the session,
which runs a turn whose assistant response is shared under the default responses policy. Finally the observer indexes Pi's own session
file and the peer reads the history back through `search_sessions`/`read_session_history`.

Models are served by a loopback OpenAI-compatible stub registered through models.json in
a private agent directory: no credentials and no real provider are used. Skips without
`pi`. Run after `task build`: python3 scripts/smoke_pi.py [--bin ./rhizome]
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
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(Path(__file__).resolve().parent))
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
from smoke_stdio import Client, until  # noqa: E402  (after sys.path)

root = Path(tempfile.mkdtemp(prefix='rzpi-'))  # short path: Unix socket names are bounded
requests = []  # bodies the stub model received


class Stub(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('content-length', 0)))
        requests.append(body.decode())
        reply = 'stub reply #%d' % len(requests)
        chunk = lambda delta, finish=None, **extra: 'data: ' + json.dumps({
            'id': 'c%d' % len(requests), 'object': 'chat.completion.chunk', 'created': 0, 'model': 'stub-1',
            'choices': [{'index': 0, 'delta': delta, 'finish_reason': finish}], **extra}) + '\n\n'
        self.send_response(200)
        self.send_header('content-type', 'text/event-stream')
        self.end_headers()
        for part in (chunk({'role': 'assistant', 'content': reply}), chunk({}, 'stop'),
                     chunk({}, None, usage={'prompt_tokens': 1, 'completion_tokens': 1, 'total_tokens': 2}), 'data: [DONE]\n\n'):
            self.wfile.write(part.encode())
        self.wfile.flush()


stub = ThreadingHTTPServer(('127.0.0.1', 0), Stub)
threading.Thread(target=stub.serve_forever, daemon=True).start()
peer = proc = None
try:
    agent, work, run, peerdir = (root / d for d in ('agent', 'work', 'run', 'peer'))
    for d in (agent, work, peerdir):
        d.mkdir(parents=True)
    run.mkdir(mode=0o700)
    (agent / 'models.json').write_text(json.dumps({'providers': {'stub': {
        'baseUrl': 'http://127.0.0.1:%d/v1' % stub.server_port, 'api': 'openai-completions', 'apiKey': 'stub',
        'models': [{'id': 'stub-1'}]}}}))
    env = {k: v for k, v in os.environ.items() if not k.startswith(('RHIZOME_', 'PI_', 'XDG_'))}
    # HOME stays real: version-manager shims for `pi` need it. Rhizome's own state is
    # isolated through an empty config file and a private runtime directory instead.
    (root / 'config.toml').write_text('')
    log = root / 'rhizome.log'
    wrapper = root / 'rhizome-wrapper'  # Pi discards child stderr; keep it for the check
    wrapper.write_text(f'#!/bin/sh\nexec "{binary}" "$@" 2>>"{log}"\n')
    wrapper.chmod(0o755)
    env.update(RHIZOME_CONFIG=str(root / 'config.toml'), PI_CODING_AGENT_DIR=str(agent), RHIZOME_RUNTIME_DIR=str(run))
    # Install exactly as users do; the wrapper stands in for the executable that setup
    # pins in rhizome.json (RHIZOME_BIN is deliberately unset), so this also checks that
    # the extension reads its launch settings and the explicit configuration.
    setup = subprocess.run([str(binary), 'setup', '--agents', 'pi'], env=env, capture_output=True, text=True)
    if setup.returncode != 0 or not (agent / 'extensions' / 'rhizome' / 'index.js').exists():
        fail('setup --agents pi failed: ' + setup.stdout[-500:] + setup.stderr[-500:])
    launch = agent / 'extensions' / 'rhizome' / 'rhizome.json'
    settings = json.loads(launch.read_text())
    if settings.get('config') != str(root / 'config.toml'):
        fail(f'setup did not pin the explicit configuration: {settings}')
    settings['command'] = str(wrapper)
    launch.write_text(json.dumps(settings))
    proc = subprocess.Popen(['pi', '--mode', 'rpc', '--provider', 'stub', '--model', 'stub-1'], cwd=work, env=env,
                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=open(root / 'pi.err', 'w'), text=True)
    responses, agent_ends = {}, []

    def drain():  # an undrained stdout pipe blocks pi
        for line in proc.stdout:
            try:
                o = json.loads(line)
            except ValueError:
                continue
            if o.get('type') == 'response':
                responses[o.get('id')] = o
            elif o.get('type') == 'agent_end':
                agent_ends.append(o)

    threading.Thread(target=drain, daemon=True).start()

    def call(i, type_, **extra):
        # Pi reads stdin only after startup; resend until the first answer arrives
        # (state queries are idempotent; prompts are sent once, below).
        deadline = time.time() + 30
        sent = 0.0
        while i not in responses:
            if time.time() > deadline:
                fail(f'no response to {type_}')
            if time.time() - sent > 2 and (not sent or type_ == 'get_state'):
                proc.stdin.write(json.dumps({'id': i, 'type': type_, **extra}) + '\n')
                proc.stdin.flush()
                sent = time.time()
            time.sleep(0.05)
        if not responses[i].get('success', True):
            fail(f'{type_} failed: {responses[i]}')
        return responses[i].get('data')

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

    # A peer in the same runtime sees the Pi session and follows it.
    # The observer and the peer share one configuration, so they agree on the log roots.
    hist = root / 'hist.toml'
    for d in ('claude', 'codex'):
        (root / d).mkdir()
    hist.write_text('runtime_dir = %s\n[claude]\nprojects_dir = %s\n[codex]\nsessions_dir = %s\n[pi]\nsessions_dir = %s\n'
                    % tuple(json.dumps(str(x)) for x in (run, root / 'claude', root / 'codex', agent / 'sessions')))
    peer = Client(peerdir, run=run, agent='claude', cwd=work, env={'RHIZOME_CONFIG': str(hist)})
    peer.bind('smoke-peer', peerdir)
    until(lambda: any(s['identity']['session_id'] == second and s['identity']['agent_type'] == 'pi' and s['status'] == 'live'
                      and s['delivery_capable'] for s in peer.tool('list_sessions')['sessions']))
    peer.tool('subscribe', {'session_id': second})
    until(lambda: peer.tool('list_sessions')['subscriptions'][second]['state'] == 'following')
    print('peer sees the live Pi session and follows it')

    # A human turn is published: the assistant text (turn ends only flush batches).
    call('4', 'prompt', message='please say something')
    ev = peer.event('stub reply #1')
    if ev['meta']['session_id'] != second:
        fail(f'activity from the wrong session: {ev}')
    print('human turn published to the peer')

    # A peer message is delivered into the session and runs a turn: the model sees it
    # framed as untrusted peer activity, and the default policy shares its response.
    peer.tool('send', {'session_id': second, 'text': 'smoke ping from peer'})
    until(lambda: len(requests) >= 2 and 'smoke ping from peer' in requests[-1])
    if '<rhizome-peer-event>' not in requests[-1]:
        fail('peer text reached the model without the peer envelope')
    until(lambda: len(agent_ends) >= 2)
    ev = peer.event('stub reply #2')
    if ev['meta']['session_id'] != second:
        fail(f'peer-triggered response from the wrong session: {ev}')
    print('peer message delivered, model saw the envelope, response shared by default')

    # History: the observer indexes the file Pi wrote (never tailing it) and the peer reads it.
    sessions = list((agent / 'sessions').rglob('*.jsonl'))
    if not sessions:
        fail('pi wrote no session file')
    observer = subprocess.Popen([str(binary), 'service', 'run', '--config', str(hist)], stdout=subprocess.DEVNULL,
                                stderr=open(root / 'observer.err', 'w'), env={k: v for k, v in env.items() if k != 'RHIZOME_CONFIG'})
    try:
        def pi_rows():  # the catalog does not exist until the observer's first flush
            result = peer.call('tools/call', {'name': 'search_sessions', 'arguments': {'agent_type': 'pi'}})
            return [] if result.get('isError') else json.loads(result['content'][0]['text'])['sessions']
        until(lambda: any(r['session_id'] == second for r in pi_rows()))
        row = next(r for r in pi_rows() if r['session_id'] == second)
        if row['agent_type'] != 'pi' or Path(row['project_path']).resolve() != work.resolve() or row['delivery_capable'] is not True:
            fail(f'unexpected history row for the live Pi session: {row}')
        page = peer.tool('read_session_history', {'session_id': second, 'agent_type': 'pi'})
        texts = [e['text'] for e in page['events'] if e.get('text')]
        if 'stub reply #1' not in texts or any('stub reply #2' in t or 'smoke ping' in t for t in texts):
            fail(f'history does not match the published activity: {page["events"]}')
        print('history indexed from Pi\'s session file and read through the peer')
    finally:
        observer.terminate()
        observer.wait(timeout=15)

    proc.stdin.close()
    proc.wait(timeout=15)
    print('OK: pi binds, collects, receives deliveries and is searchable through the installed extension')
finally:
    if peer is not None:
        try:
            peer.close()
        except Exception:
            pass
    if proc is not None and proc.poll() is None:
        proc.kill()
    stub.shutdown()
    shutil.rmtree(root, ignore_errors=True)
