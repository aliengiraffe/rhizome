#!/usr/bin/env python3
"""Persistent privacy across real MCP processes, CLI and the observer.
Run after task build, with disposable HOME as in AGENTS.md.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile
from smoke_stdio import Client, ROOT, until
from smoke_service import historical


def main():
    with tempfile.TemporaryDirectory(prefix='rhizome-privacy-') as tmp:
        directory = Path(tmp)
        logs = directory / 'logs'
        logs.mkdir()
        config = directory / 'smoke-config.toml'
        config.write_text('hostname = "Privacy smoke"\n[claude]\nprojects_dir = ' + json.dumps(str(logs)) + '\n[update]\nenabled = false\n')
        runtime = directory / 'run'
        env = {**os.environ, 'RHIZOME_CONFIG': str(config), 'RHIZOME_RUNTIME_DIR': str(runtime)}
        source = logs / 'alpha.jsonl'
        historical(source, 'alpha', '2026-01-01T00:00:00Z', 'u', 'human', 'user')
        historical(source, 'alpha', '2026-01-01T00:00:01Z', 'a', 'old answer')
        service_log = tempfile.TemporaryFile()
        service = subprocess.Popen([str(ROOT / 'rhizome'), 'service', 'run'], env=env, stdout=service_log, stderr=service_log)
        clients = []
        try:
            until(lambda: (runtime / 'history' / 'catalog.json').exists())
            assert service.poll() is None
            a, b = Client(directory), Client(directory)
            clients.extend([a, b])
            a.bind('alpha', logs)
            b.bind('beta', directory)
            until(lambda: any(s['identity']['session_id'] == 'alpha' for s in b.tool('list_sessions')['sessions']))
            until(lambda: len(b.tool('search_sessions', {'session_id': 'alpha'})['sessions']) == 1)
            b.tool('subscribe', {'session_id': 'alpha'})
            until(lambda: b.tool('list_sessions')['subscriptions'].get('alpha', {}).get('state') == 'following')
            a.tool('publish', {'text': 'before privacy'})
            b.event('before privacy')
            result = a.tool('set_session_visibility', {'visibility': 'private'})
            assert result['applied'] and result['visibility'] == 'private', result
            assert not b.tool('search_sessions', {'session_id': 'alpha'})['sessions']
            assert b.error('read_session_history', {'session_id': 'alpha', 'agent_type': 'claude'})['code'] == 'SESSION_NOT_FOUND'
            assert not [s for s in b.tool('list_sessions')['sessions'] if s['identity']['session_id'] == 'alpha']
            assert 'alpha' not in b.tool('list_sessions')['subscriptions']
            a.tool_error('publish', {'text': 'private marker'})
            a.tool_error('set_session_visibility', {'visibility': 'public', 'session_id': 'beta'})
            a.close()
            clients.remove(a)
            historical(source, 'alpha', '2026-01-01T00:00:02Z', 'secret', 'private interval')
            # The observer must not take over publication when the direct process exits.
            assert not b.tool('search_sessions', {'session_id': 'alpha'})['sessions']
            a = Client(directory)
            clients.append(a)
            a.bind('alpha', logs)
            assert a.tool('list_sessions')['visibility'] == 'private'
            state = json.loads(subprocess.check_output([str(ROOT / 'rhizome'), 'session', 'visibility', 'alpha'], env=env))
            assert state['visibility'] == 'private' and state['applied'], state
            result = json.loads(subprocess.check_output([str(ROOT / 'rhizome'), 'session', 'visibility', 'alpha', 'public'], env=env))
            assert result['applied'], result
            until(lambda: any(s['identity']['session_id'] == 'alpha' for s in b.tool('list_sessions')['sessions']))
            assert 'alpha' not in b.tool('list_sessions')['subscriptions']
            b.tool('subscribe', {'session_id': 'alpha'})
            until(lambda: b.tool('list_sessions')['subscriptions'].get('alpha', {}).get('state') == 'following')
            a.tool('publish', {'text': 'after privacy'})
            b.event('after privacy')
            assert 'private interval' not in json.dumps(b.events)
            service.terminate()
            assert service.wait(timeout=10) == 0
            subprocess.check_call([str(ROOT / 'rhizome'), 'session', 'visibility', 'alpha', 'private'], env=env)
            assert not b.tool('search_sessions', {'session_id': 'alpha'})['sessions']
        finally:
            for client in clients:
                client.close()
            if service.poll() is None:
                service.terminate()
                service.wait(timeout=10)
            service_log.close()
    print('PASS: visibility revocation, observer handoff, restart persistence, CLI, stale history and explicit re-subscription')


if __name__ == '__main__':
    main()
