#!/usr/bin/env python3
"""Cursor integration smoke with synthetic hook payloads. Run after task build.

Drives `rhizome mcp --agent cursor` the way Cursor would: MCP over stdio for
tools, and `rhizome hook --agent cursor` subprocesses fed the hook payloads
Cursor writes on stdin. A Claude-mode peer verifies cross-agent discovery,
subscriptions in both directions, direct messages, hook-pull delivery, ticket
attribution with two conversations, peer-turn suppression, default-on followup
delivery and its opt-out, and registration cleanup. No Cursor installation is needed and no
model turn runs; the live Cursor checklist stays in docs/verification.md.
"""
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
BINARY = str(ROOT / 'rhizome')


def until(check, what='condition', timeout=10):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(.05)
    raise AssertionError(what + ' timed out')


class Client:
    def __init__(self, directory, agent, config_body='', cwd=None):
        self.run = directory / 'run'
        self.config = directory / ('smoke-config-%s.toml' % (agent if not config_body else 'followup'))
        if not self.config.exists():
            self.config.touch(mode=0o600)
            self.config.write_text('hostname = "Smoke device"\n' + config_body)
        self.env = {**os.environ, 'RHIZOME_CONFIG': str(self.config), 'RHIZOME_RUNTIME_DIR': str(self.run)}
        self.log = tempfile.TemporaryFile()
        # Cursor starts MCP servers in the workspace (CLI) or the home directory (IDE); the
        # registration records that directory so hooks can find the process before roots/list.
        self.proc = subprocess.Popen([BINARY, 'mcp', '--agent', agent], stdin=subprocess.PIPE,
                                     stdout=subprocess.PIPE, stderr=self.log, text=True, env=self.env, cwd=cwd)
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
                                      'clientInfo': {'name': 'cursor-smoke', 'version': '1'}})
        channels = init['capabilities'].get('experimental', {}).get('claude/channel')
        assert (channels == {}) == (agent == 'claude'), init
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

    def raw_tool(self, name, args=None):
        return self.call('tools/call', {'name': name, 'arguments': args or {}})

    def tool(self, name, args=None):
        result = self.raw_tool(name, args)
        assert not result.get('isError'), result
        return json.loads(result['content'][0]['text'])

    def error_text(self, name, args=None):
        result = self.raw_tool(name, args)
        assert result.get('isError'), result
        return result['content'][0]['text']

    def bind(self, session, directory):
        self.tool('observe_session', {'session_id': session, 'transcript_path': str(directory / (session + '.jsonl')),
                                    'cwd': str(directory), 'hook_event_name': 'UserPromptSubmit'})

    def event(self, needle, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for i, msg in enumerate(self.events):
                if msg.get('method') == 'notifications/claude/channel' and needle in msg['params']['content']:
                    return self.events.pop(i)['params']
            try:
                self.events.append(self.messages.get(timeout=max(.1, deadline - time.monotonic())))
            except queue.Empty:
                break
        raise AssertionError('notification missing: ' + needle)

    def saw(self, needle):
        while True:
            try:
                self.events.append(self.messages.get_nowait())
            except queue.Empty:
                break
        return any(m.get('method') == 'notifications/claude/channel' and needle in m['params']['content'] for m in self.events)

    def hook(self, payload):
        """Run `rhizome hook --agent cursor` exactly as Cursor's hooks.json would."""
        proc = subprocess.run([BINARY, 'hook', '--agent', 'cursor'], input=json.dumps(payload), capture_output=True,
                              text=True, env=self.env, timeout=15)
        assert proc.returncode == 0, proc.stderr
        lines = proc.stdout.splitlines()
        assert len(lines) == 1, proc.stdout
        return json.loads(lines[0])

    def close(self):
        self.proc.stdin.close()
        assert self.proc.wait(timeout=5) == 0
        self.reader.join(timeout=1)
        self.proc.stdout.close()
        self.log.close()


def payload(event, conversation, directory, **extra):
    return {'hook_event_name': event, 'conversation_id': conversation, 'generation_id': 'gen', 'model': 'smoke',
            'cursor_version': '0.0.0-smoke', 'workspace_roots': [str(directory)], 'transcript_path': None,
            'user_email': 'nobody@example.com', **extra}


def main():
    clients = []
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        project = directory / 'proj'
        project.mkdir()
        try:
            c = Client(directory, 'cursor', cwd=str(project))
            clients.append(c)
            tools = {t['name'] for t in c.call('tools/list', {})['tools']}
            assert 'observe_session' not in tools and {'list_sessions', 'send', 'subscribe'} <= tools, tools
            assert 'UNBOUND' in c.error_text('list_sessions')
            # sessionStart binds and injects static instructions only.
            start = c.hook(payload('sessionStart', 'conv-a', project, session_id='conv-a', is_background_agent=False))
            assert 'untrusted' in start['additional_context'] and 'rhizome-peer-event' in start['additional_context'], start
            state = c.tool('list_sessions')
            identity = state['identity']
            assert state['binding'] == 'BOUND' and identity['agent_type'] == 'cursor' and identity['session_id'] == 'conv-a'
            assert identity['project'] == str(project) and identity['session_name'] == 'proj · cursor · conv-a', identity
            assert state['delivery_mode'] == 'hook_pull' and state['collection_state'] == 'hooks', state

            p = Client(directory, 'claude')
            clients.append(p)
            p.bind('alpha', directory)

            def sees(client, session, agent):
                return any(s['identity']['session_id'] == session and s['identity']['agent_type'] == agent
                           and s['status'] == 'live' for s in client.tool('list_sessions').get('sessions', []))
            until(lambda: sees(p, 'conv-a', 'cursor'), 'claude sees cursor')
            until(lambda: sees(c, 'alpha', 'claude'), 'cursor sees claude')

            # Claude subscribes to the Cursor conversation; hook-pushed activity flows out.
            p.tool('subscribe', {'session_id': 'conv-a'})
            until(lambda: p.tool('list_sessions')['subscriptions']['conv-a']['state'] == 'following', 'following')
            assert c.hook(payload('afterAgentResponse', 'conv-a', project, text='cursor-marker')) == {}
            assert p.event('cursor-marker')['meta']['session_id'] == 'conv-a'
            drained = c.hook(payload('postToolUse', 'conv-a', project, tool_name='Shell', tool_input={'command': 'ls'},
                                     tool_output='{"exitCode":0}', cwd=str(project), duration=5))
            assert drained == {}, drained
            assert 'Tool completed: Shell' in p.event('Tool completed')['content']
            assert c.hook(payload('stop', 'conv-a', project, status='completed', loop_count=0)) == {}

            # Direct message into the Cursor conversation is pulled at the next tool boundary.
            p.tool('send', {'session_id': 'conv-a', 'text': 'direct-to-cursor'})

            def drain():
                resp = c.hook(payload('postToolUse', 'conv-a', project, tool_name='Read', tool_input={}, tool_output='', cwd=str(project)))
                return resp.get('additional_context', '')
            injected = until(lambda: 'direct-to-cursor' in drain() and drain or None, 'hook-pull delivery')
            # The injected block was consumed by the successful drain inside until(); verify framing on the next event.
            c.tool('subscribe', {'session_id': 'alpha'})
            until(lambda: c.tool('list_sessions')['subscriptions']['alpha']['state'] == 'following', 'cursor following claude')
            p.tool('publish', {'text': 'from-claude'})
            block = until(lambda: (lambda r: r if 'from-claude' in r else None)(drain()), 'announcement delivery')
            assert '<rhizome-peer-event>' in block and '"kind":"announcement"' in block and 'untrusted' in block, block
            assert 'Tool completed: Read' in p.event('Tool completed: Read')['content']

            # Followup delivery is on by default: one auto-submitted message per
            # completed turn with loop_count 0, and the follow-up turn is peer-only.
            p.tool('send', {'session_id': 'conv-a', 'text': 'queued-for-stop'})
            until(lambda: c.tool('list_sessions')['outbox_depth'] == 1, 'outbox')
            assert 'followup_message' not in c.hook(payload('stop', 'conv-a', project, status='completed', loop_count=1))
            assert 'followup_message' not in c.hook(payload('stop', 'conv-a', project, status='aborted', loop_count=0))
            follow = c.hook(payload('stop', 'conv-a', project, status='completed', loop_count=0))['followup_message']
            summary, _, rest = follow.partition('\n')
            assert summary.startswith('Rhizome: 1 message from ') and '"queued-for-stop"' in summary, follow
            assert rest.startswith('Rhizome delivered queued peer activity') and 'queued-for-stop' in rest, follow
            assert 'followup_message' not in c.hook(payload('stop', 'conv-a', project, status='completed', loop_count=0))
            c.hook(payload('afterAgentResponse', 'conv-a', project, text='followup-reply'))
            c.hook(payload('beforeSubmitPrompt', 'conv-a', project, prompt='next real prompt', attachments=[]))
            c.hook(payload('afterAgentResponse', 'conv-a', project, text='after-followup'))
            p.event('after-followup')
            assert not p.saw('followup-reply'), 'followup turn leaked to subscribers'

            # Two conversations in one process: tools fail closed without a ticket.
            c.hook(payload('sessionStart', 'conv-b', project, session_id='conv-b', is_background_agent=False))
            assert 'identity_ambiguous' in c.error_text('list_sessions')
            ticket = c.hook(payload('beforeMCPExecution', 'conv-b', project, tool_name='list_sessions', tool_input='{}',
                                    mcp_server_name='rhizome', command=BINARY))
            assert ticket == {'permission': 'allow'}, ticket
            assert c.tool('list_sessions')['identity']['session_id'] == 'conv-b'
            assert 'identity_ambiguous' in c.error_text('list_sessions')
            # The IDE fires only preToolUse, naming the tool "MCP:<tool>"; approval stays with Cursor.
            assert c.hook(payload('preToolUse', 'conv-b', project, tool_name='MCP:list_sessions', tool_input={})) == {}
            assert c.tool('list_sessions')['identity']['session_id'] == 'conv-b'
            assert c.hook(payload('preToolUse', 'conv-b', project, tool_name='Shell', tool_input={'command': 'ls'})) == {}
            assert 'identity_ambiguous' in c.error_text('list_sessions')
            other = c.hook(payload('beforeMCPExecution', 'conv-b', project, tool_name='x', tool_input='{}', mcp_server_name='github'))
            assert other == {'permission': 'ask'}, other
            assert 'identity_ambiguous' in c.error_text('list_sessions')
            # Subagent payloads never bind or publish.
            assert c.hook(payload('afterAgentResponse', 'conv-sub', project, text='sub-marker', subagent_id='s1')) == {}
            until(lambda: len([s for s in p.tool('list_sessions')['sessions'] if s['identity']['agent_type'] == 'cursor']) == 2, 'two cursor sessions')
            assert all(s['identity']['session_id'] != 'conv-sub' for s in p.tool('list_sessions')['sessions'])

            # A turn started by a peer envelope is suppressed until the next real prompt.
            envelope = 'Rhizome peer activity.\n<rhizome-peer-event>\n{}\n</rhizome-peer-event>'
            assert c.hook(payload('beforeSubmitPrompt', 'conv-a', project, prompt=envelope, attachments=[])) == {'continue': True}
            c.hook(payload('afterAgentResponse', 'conv-a', project, text='suppressed-marker'))
            c.hook(payload('stop', 'conv-a', project, status='completed', loop_count=0))
            assert c.hook(payload('beforeSubmitPrompt', 'conv-a', project, prompt='real prompt mentioning rhizome', attachments=[])) == {'continue': True}
            c.hook(payload('afterAgentResponse', 'conv-a', project, text='visible-marker'))
            p.event('visible-marker')
            assert not p.saw('suppressed-marker'), 'peer-only turn leaked to subscribers'

            # sessionEnd retires conv-b: the process is unambiguous again.
            assert c.hook(payload('sessionEnd', 'conv-b', project, session_id='conv-b', reason='user_close', duration_ms=1)) == {}
            assert c.tool('list_sessions')['identity']['session_id'] == 'conv-a'
            until(lambda: len([s for s in p.tool('list_sessions')['sessions'] if s['identity']['agent_type'] == 'cursor']) == 1, 'conv-b gone')

            # A second Cursor process (the CLI case: no roots/list, started in its
            # workspace) coexists: hooks from each workspace reach their own process.
            other = directory / 'other'
            other.mkdir()
            g = Client(directory, 'cursor', cwd=str(other))
            clients.append(g)
            g.hook(payload('sessionStart', 'conv-g', other, session_id='conv-g', is_background_agent=False))
            assert g.tool('list_sessions')['identity']['session_id'] == 'conv-g'
            assert g.tool('list_sessions')['cursor_processes'] == 2
            assert c.tool('list_sessions')['cursor_processes'] == 2
            assert c.tool('list_sessions')['identity']['session_id'] == 'conv-a'
            # A workspace neither process serves binds nowhere and leaves both untouched.
            elsewhere = directory / 'elsewhere'
            elsewhere.mkdir()
            assert c.hook(payload('sessionStart', 'conv-x', elsewhere, session_id='conv-x', is_background_agent=False)) == {}
            until(lambda: sees(p, 'conv-g', 'cursor'), 'claude sees the second process')
            assert all(s['identity']['session_id'] != 'conv-x' for s in p.tool('list_sessions')['sessions'])
            g.close()
            clients.remove(g)

            # Followup delivery disabled by configuration: the stop hook never
            # auto-submits and queued events wait for the next tool result.
            c.close()
            clients.remove(c)
            until(lambda: not list((directory / 'run' / 'cursor').glob('*')), 'registration cleanup')
            f = Client(directory, 'cursor', '[cursor]\nfollowup_delivery = false\n', cwd=str(project))
            clients.append(f)
            f.hook(payload('sessionStart', 'conv-f', project, session_id='conv-f', is_background_agent=False))
            until(lambda: sees(p, 'conv-f', 'cursor'), 'claude sees the opt-out process')
            p.tool('send', {'session_id': 'conv-f', 'text': 'no-followup-marker'})
            until(lambda: f.tool('list_sessions')['outbox_depth'] == 1, 'opt-out outbox')
            assert 'followup_message' not in f.hook(payload('stop', 'conv-f', project, status='completed', loop_count=0))
            drained = f.hook(payload('postToolUse', 'conv-f', project, tool_name='Read', tool_input={}, tool_output='', cwd=str(project)))
            assert 'no-followup-marker' in drained.get('additional_context', ''), drained
        finally:
            for client in clients:
                client.close()
        assert not list(directory.glob('run/*.json')), 'EOF left registrations'
        assert not list(directory.glob('run/cursor/*')), 'EOF left Cursor registrations or sockets'
    print('PASS: Cursor hook binding, roots-free registration, cross-agent subscriptions, hook-pull delivery, '
          'tickets (beforeMCPExecution and IDE preToolUse) with two conversations, two workspaces on two processes, peer-turn suppression, default-on followup and its opt-out, sessionEnd and cleanup')


if __name__ == '__main__':
    main()
