#!/usr/bin/env python3
"""Real-process MCP framing, routing, restart and EOF checks. Run after task build.

With --lan (or RHIZOME_SMOKE_LAN=1) every process gets its own runtime directory,
as separate devices would, shares one token, binds LAN mode to loopback with mDNS
off, and seeds gossip with --join; this exercises pinned TLS and cross-registry
discovery on one host.
"""
import json
import os
from pathlib import Path
import queue
import shutil
import subprocess
import sys
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
LAN = '--lan' in sys.argv[1:] or os.environ.get('RHIZOME_SMOKE_LAN') == '1'

class Client:
    count = 0

    def __init__(self, directory, join=None, run=None):
        Client.count += 1
        if run is None:  # A restarted process on one device keeps its runtime directory.
            run = directory / ('run-%d' % Client.count if LAN else 'run')
        self.run = run
        args = [str(ROOT / 'rhizome'), 'mcp']
        env = {**os.environ, 'RHIZOME_RUNTIME_DIR': str(run)}
        token = directory / 'token'
        if LAN:
            args += ['--lan', '--bind-addr', '127.0.0.1', '--mdns=false']
            if join:
                args += ['--join', join]
            if token.exists():
                env['RHIZOME_TOKEN_FILE'] = str(token)
        self.log = tempfile.TemporaryFile()
        self.proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=self.log, text=True, env=env)
        self.messages = queue.Queue()
        self.events = []
        self.number = 0
        def read():
            try:
                for line in self.proc.stdout:
                    self.messages.put(json.loads(line))  # Any stdout log is a failure.
            except Exception as error:
                self.messages.put(error)
        self.reader = threading.Thread(target=read, daemon=True)
        self.reader.start()
        init = self.call('initialize', {'protocolVersion': '2025-11-25', 'capabilities': {},
                                      'clientInfo': {'name': 'stdio-smoke', 'version': '1'}})
        assert init['protocolVersion'] == '2025-11-25'
        assert init['capabilities']['experimental']['claude/channel'] == {}
        self.write({'method': 'notifications/initialized'})
        if LAN and not token.exists():
            shutil.copy(run / 'token', token)  # The first device's token seeds the rest.

    def gossip(self):
        identity = self.tool('list_sessions')['identity']
        if LAN:
            assert len(identity['cert_fingerprint']) == 64 and identity['host'], identity
        else:
            assert 'cert_fingerprint' not in identity, identity
        return identity['gossip_endpoint']

    def write(self, obj):
        self.proc.stdin.write(json.dumps({'jsonrpc': '2.0', **obj}) + '\n')
        self.proc.stdin.flush()

    def call(self, method, params):
        self.number += 1
        request_id = self.number
        self.write({'id': request_id, 'method': method, 'params': params})
        while True:
            msg = self.messages.get(timeout=10)
            assert not isinstance(msg, Exception), msg
            if msg.get('id') == request_id:
                assert 'error' not in msg, msg
                return msg['result']
            self.events.append(msg)

    def tool(self, name, args=None):
        result = self.call('tools/call', {'name': name, 'arguments': args or {}})
        assert not result.get('isError'), result
        return json.loads(result['content'][0]['text'])

    def bind(self, session, directory):
        self.tool('observe_session', {'session_id': session, 'transcript_path': str(directory / (session + '.jsonl')),
                                    'cwd': str(directory), 'hook_event_name': 'UserPromptSubmit'})

    def event(self, needle):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            for i, msg in enumerate(self.events):
                if msg.get('method') == 'notifications/claude/channel' and needle in msg['params']['content']:
                    return self.events.pop(i)['params']
            self.events.append(self.messages.get(timeout=max(.1, deadline - time.monotonic())))
        raise AssertionError('notification missing: ' + needle)

    def close(self):
        self.proc.stdin.close()
        assert self.proc.wait(timeout=5) == 0
        self.reader.join(timeout=1)
        self.proc.stdout.close()
        self.log.close()

def until(check):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(.05)
    raise AssertionError('condition timed out')

def main():
    clients = []
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        try:
            a = Client(directory)
            b = Client(directory, join=a.gossip())
            clients += [a, b]
            assert a.tool('list_sessions')['binding'].startswith('UNBOUND')
            a.bind('alpha', directory)
            b.bind('beta', directory)
            until(lambda: len(b.tool('list_sessions')['sessions']) == 2)
            fallback = directory.name + ' \u00b7 claude \u00b7 beta'
            assert b.tool('list_sessions')['identity']['session_name'] == fallback
            assert a.tool('set_session_name', {'name': '  alpha-task-name  '})['session_name'] == 'alpha-task-name'
            assert a.call('tools/call', {'name': 'set_session_name', 'arguments': {'name': 'x' * 97}})['isError']
            def named(client, session, name):
                return any(s['identity']['session_id'] == session and s['identity'].get('session_name') == name
                           for s in client.tool('list_sessions')['sessions'])
            until(lambda: named(b, 'alpha', 'alpha-task-name'))
            b.tool('subscribe', {'session_id': 'alpha'})
            until(lambda: b.tool('list_sessions')['subscriptions']['alpha']['state'] == 'following')
            a.tool('publish', {'text': 'announcement-marker'})
            assert b.event('announcement-marker')['meta']['session_id'] == 'alpha'
            a.tool('send', {'session_id': 'beta', 'text': 'direct-marker'})
            assert b.event('direct-marker')['meta']['kind'] == 'direct'
            with (directory / 'alpha.jsonl').open('a') as transcript:
                transcript.write(json.dumps({'type': 'assistant', 'uuid': 'one', 'message': {
                    'content': [{'type': 'text', 'text': 'assistant-marker'}], 'stop_reason': 'end_turn'}}) + '\n')
            assert b.event('assistant-marker')['meta']['session_id'] == 'alpha'
            a.close()
            clients.remove(a)
            a = Client(directory, join=b.gossip(), run=a.run)
            clients.append(a)
            a.bind('alpha', directory)
            # Explicit names survive a publisher process restart.
            assert a.tool('list_sessions')['identity']['session_name'] == 'alpha-task-name'
            assert b.event('publisher restarted')['meta']['kind'] == 'gap'
            assert a.tool('set_session_name', {'name': ''})['session_name'] == directory.name + ' \u00b7 claude \u00b7 alpha'
            until(lambda: named(b, 'alpha', directory.name + ' \u00b7 claude \u00b7 alpha'))
            b.tool('unsubscribe', {'session_id': 'alpha'})
            b.bind('gamma', directory)
            assert b.tool('list_sessions')['identity']['generation'] == 2
            # Old lifecycle hooks cannot roll a generation backwards.
            result = b.call('tools/call', {'name': 'observe_session', 'arguments': {
                'session_id': 'beta', 'transcript_path': str(directory / 'beta.jsonl'),
                'cwd': str(directory), 'hook_event_name': 'SessionStart'}})
            assert result['isError']
        finally:
            for client in clients:
                client.close()
        assert not list(directory.glob('run*/*.json')), 'EOF left registrations'
    print('PASS%s: MCP stdout, unbound startup, two-session routing, session names, transcript, restart gap, rebind and EOF'
          % (' (LAN mode, pinned TLS, separate registries)' if LAN else ''))

if __name__ == '__main__':
    main()
