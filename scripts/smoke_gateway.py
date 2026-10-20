#!/usr/bin/env python3
"""Isolated services with distinct tokens: pairing, routing, reconnect and revocation."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile

from smoke_stdio import Client, ROOT, until


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def main():
    with tempfile.TemporaryDirectory() as tmp, tempfile.TemporaryFile() as log:
        hosts, clients, processes = [], [], []
        base = Path(tmp)
        # Remove ambient overrides: the smoke must never connect to a real gateway.
        env = {k: v for k, v in os.environ.items() if not k.startswith('RHIZOME_')}

        def command(host, *args, ok=True):
            result = subprocess.run([str(ROOT / 'rhizome'), *args], env={**env, 'RHIZOME_CONFIG': str(host['config'])},
                                    capture_output=True, text=True, timeout=30)
            if ok:
                assert result.returncode == 0, result.stderr
            else:
                assert result.returncode != 0, 'unexpected success'
            return result.stdout.strip()

        def start(host):
            process = subprocess.Popen([str(ROOT / 'rhizome'), 'service', 'run', '--config', str(host['config'])],
                                       env=env, stdout=log, stderr=log)
            host['process'] = process
            processes.append(process)
            until(lambda: (host['run'] / 'gateway' / 'control.json').exists())
            command(host, 'peers', 'list')

        def sessions(client):
            return {s['identity']['session_id'] for s in client.tool('list_sessions')['sessions']}

        try:
            for name in ('a', 'b', 'c'):
                directory = base / name
                directory.mkdir()
                config = directory / 'config.toml'
                run = directory / 'run'
                config.write_text('runtime_dir = %s\nclaude.projects_dir = %s\ncodex.sessions_dir = %s\n'
                                  'gateway.mode = "lan"\ngateway.listen = "127.0.0.1:%d"\n' %
                                  (json.dumps(str(run)), json.dumps(str(directory / 'claude')),
                                   json.dumps(str(directory / 'codex')), port()))
                host = {'config': config, 'run': run, 'name': name}
                hosts.append(host)
                start(host)
                client = Client(directory, run=run, env={**{k: '' for k in os.environ if k.startswith('RHIZOME_')}, 'RHIZOME_CONFIG': str(config)})
                client.bind('gateway-' + name, directory)
                clients.append(client)
            a, b, c = hosts
            ca, cb, cc = clients
            tokens = [(h['run'] / 'token').read_bytes() for h in hosts]
            assert len(set(tokens)) == 3
            code = command(a, 'invite').split()[-1]
            paired = json.loads(command(b, 'join', code))
            command(c, 'join', code, ok=False)
            until(lambda: 'gateway-b' in sessions(ca) and 'gateway-a' in sessions(cb))
            ca.tool('send', {'session_id': 'gateway-b', 'text': 'gateway forward'})
            cb.event('gateway forward')
            cb.tool('send', {'session_id': 'gateway-a', 'text': 'gateway reverse'})
            ca.event('gateway reverse')
            cb.tool('subscribe', {'session_id': 'gateway-a'})
            until(lambda: cb.tool('list_sessions')['subscriptions']['gateway-a']['state'] == 'following')
            # Synthetic transcript append exercises the existing normalizer through the gateway.
            with (base / 'a' / 'gateway-a.jsonl').open('a') as stream:
                stream.write(json.dumps({'type': 'assistant', 'uuid': 'gateway-activity',
                                         'message': {'content': 'activity through paired hosts'}}) + '\n')
            ca.bind('gateway-a', base / 'a')
            cb.event('activity through paired hosts')
            # Persistent trust reconnects after a service restart; tokens remain untouched.
            a['process'].terminate()
            a['process'].wait(timeout=20)
            start(a)
            until(lambda: 'gateway-a' in sessions(cb))
            until(lambda: json.loads(command(b, 'peers', 'list'))[0]['connected'])
            cb.tool('send', {'session_id': 'gateway-a', 'text': 'after gateway restart'})
            ca.event('after gateway restart')
            command(c, 'join', command(b, 'invite').split()[-1])
            until(lambda: 'gateway-b' in sessions(cc) and 'gateway-c' in sessions(cb))
            assert 'gateway-a' not in sessions(cc)
            assert 'gateway-c' not in sessions(ca)
            command(b, 'peers', 'revoke', paired['host_id'])
            until(lambda: 'gateway-a' not in sessions(cb))
            assert cb.call('tools/call', {'name': 'send', 'arguments': {'session_id': 'gateway-a', 'text': 'must fail'}}).get('isError')
            assert [(h['run'] / 'token').read_bytes() for h in hosts] == tokens
            print('gateway smoke: PASS (pairing, distinct tokens, bidirectional routing, activity, restart, non-transitivity, revocation)')
        finally:
            for client in clients:
                client.close()
            for process in processes:
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=20)


if __name__ == '__main__':
    main()
