#!/usr/bin/env python3
"""Opt-in live test: creates two disposable Codex threads and runs small model turns.
Requires a running local daemon, authenticated Codex, and `make build`.
Does not trust hooks, change config, or start/restart the daemon.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import struct
import subprocess
import tempfile
import time

from smoke_stdio import Client, ROOT


class AppServer:
    def __init__(self, path):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(30)
        self.sock.connect(str(path))
        key = base64.b64encode(os.urandom(16))
        self.sock.sendall(b'GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\n'
                          b'Connection: Upgrade\r\nSec-WebSocket-Key: ' + key +
                          b'\r\nSec-WebSocket-Version: 13\r\n\r\n')
        header = b''
        while not header.endswith(b'\r\n\r\n'):
            header += self.read(1)
            assert len(header) < 8192, 'oversized handshake'
        accept = base64.b64encode(hashlib.sha1(key + b'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest())
        assert b'101 Switching Protocols' in header and accept in header
        self.seq = 0
        self.call('initialize', {'clientInfo': {'name': 'rhizome_smoke', 'version': '1'},
                                 'capabilities': {'experimentalApi': True}})
        self.write({'method': 'initialized'})

    def read(self, count):
        data = b''
        while len(data) < count:
            part = self.sock.recv(count - len(data))
            if not part:
                raise EOFError('App Server disconnected')
            data += part
        return data

    def write(self, obj, opcode=1):
        data = json.dumps(obj).encode() if opcode == 1 else obj
        mask = os.urandom(4)
        head = bytes([128 | opcode, 128 | len(data)]) if len(data) < 126 else bytes([128 | opcode, 254]) + struct.pack('!H', len(data))
        self.sock.sendall(head + mask + bytes(c ^ mask[i % 4] for i, c in enumerate(data)))

    def call(self, method, params):
        self.seq += 1
        self.write({'id': self.seq, 'method': method, 'params': params})
        while True:
            head = self.read(2)
            size = head[1] & 127
            if size == 126:
                size = struct.unpack('!H', self.read(2))[0]
            elif size == 127:
                size = struct.unpack('!Q', self.read(8))[0]
            assert size <= 8 * 1024 * 1024 and not head[1] & 128
            data = self.read(size)
            if head[0] & 15 == 9:
                self.write(data, opcode=10)
                continue
            assert head[0] == 129, 'unexpected WebSocket frame'
            obj = json.loads(data)
            if obj.get('id') == self.seq:
                assert 'error' not in obj, obj
                return obj['result']

    def tool(self, thread, name, arguments=None):
        result = self.call('mcpServer/tool/call', {'threadId': thread, 'server': 'rhizome',
                          'tool': name, 'arguments': arguments or {},
                          '_meta': {'threadId': 'deliberately-conflicting-client-metadata'}})
        assert not result.get('isError'), result
        return json.loads(result['content'][0]['text'])


def wait(check, seconds=45):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(.2)
    raise AssertionError('live smoke timed out')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--live', action='store_true', help='allow disposable threads and model turns')
    args = parser.parse_args()
    if not args.live:
        parser.error('pass --live to run the model-backed smoke test')
    binary = shutil.which('codex')
    assert binary, 'codex not on PATH'
    home = Path(os.environ.get('CODEX_HOME', str(Path.home() / '.codex')))
    app = AppServer(home / 'app-server-control/app-server-control.sock')
    threads = []
    with tempfile.TemporaryDirectory(prefix='rhizome-codex-smoke-') as tmp:
        directory = Path(tmp)
        claude = Client(directory)
        try:
            claude.bind('claude-protocol-smoke', directory)
            config = {'mcp_servers.rhizome': {'command': str(ROOT / 'rhizome'),
                      'args': ['mcp', '--agent', 'codex', '--codex-bin', binary],
                      'env': {'CODEX_HOME': str(home), 'RHIZOME_RUNTIME_DIR': str(directory / 'run')}}}
            for _ in range(2):
                thread = app.call('thread/start', {'cwd': str(directory), 'config': config,
                         'baseInstructions': 'Rhizome integration test. Reply only ACK to input. Never use tools.'})['thread']['id']
                threads.append(thread)
                # A trusted SessionStart may already have bound it. Explicit
                # synthetic hooks here exercise routing; the TUI tests real hooks.
                app.tool(thread, 'observe_session', {'session_id': thread, 'transcript_path': None,
                         'cwd': str(directory), 'hook_event_name': 'UserPromptSubmit'})
                state = app.tool(thread, 'list_sessions')
                assert state['identity']['session_id'] == thread
            a, b = threads
            wait(lambda: len(app.tool(a, 'list_sessions').get('sessions', [])) >= 3)
            app.tool(a, 'send', {'session_id': 'claude-protocol-smoke', 'text': 'Codex to Claude smoke'})
            event = claude.event('Codex to Claude smoke')
            assert event['meta']['session_id'] == a
            claude.tool('subscribe', {'session_id': b})
            wait(lambda: claude.tool('list_sessions')['subscriptions'][b]['state'] == 'following')
            app.tool(a, 'send', {'session_id': b, 'text': 'Rhizome smoke; no action requested.'})
            wait(lambda: app.tool(b, 'list_sessions')['counters']['QueueHandoffs'] == 1)
            # Peer-triggered output is intentionally suppressed. Exercise normal
            # collection with a separate input that has no peer envelope.
            wait(lambda: app.call('thread/read', {'threadId': b})['thread']['status']['type'] == 'idle')
            subprocess.run([binary, 'queue', '--remote', 'unix://' + str(home / 'app-server-control/app-server-control.sock'),
                            '--thread', b, '--message', 'Human test input: reply ACK.'],
                           check=True, capture_output=True, timeout=10)
            event = claude.event('ACK')
            assert event['meta']['session_id'] == b
            claude.tool('send', {'session_id': a, 'text': 'Claude to Codex smoke; no action requested.'})
            wait(lambda: app.tool(a, 'list_sessions')['counters']['QueueHandoffs'] == 1)
            print('PASS: exact-thread routing, queue handoff, real assistant collection, and bidirectional Claude protocol delivery')
        finally:
            claude.close()
            for thread in threads:
                app.call('thread/archive', {'threadId': thread})
            app.sock.close()


if __name__ == '__main__':
    main()
