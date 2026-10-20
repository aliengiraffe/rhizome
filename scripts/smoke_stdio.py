#!/usr/bin/env python3
"""Real-process MCP framing, routing, restart and EOF checks. Run after task build."""
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]

class Client:
    def __init__(self, directory):
        self.log = tempfile.TemporaryFile()
        self.proc = subprocess.Popen([str(ROOT / 'rhizome'), 'mcp'], stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=self.log, text=True,
                                     env={**os.environ, 'RHIZOME_RUNTIME_DIR': str(directory / 'run')})
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
            a, b = Client(directory), Client(directory)
            clients += [a, b]
            assert a.tool('list_sessions')['binding'].startswith('UNBOUND')
            a.bind('alpha', directory)
            b.bind('beta', directory)
            until(lambda: len(b.tool('list_sessions')['sessions']) == 2)
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
            a = Client(directory)
            clients.append(a)
            a.bind('alpha', directory)
            assert b.event('publisher restarted')['meta']['kind'] == 'gap'
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
        assert not list((directory / 'run').glob('*.json')), 'EOF left registrations'
    print('PASS: MCP stdout, unbound startup, two-session routing, transcript, restart gap, rebind and EOF')

if __name__ == '__main__':
    main()
