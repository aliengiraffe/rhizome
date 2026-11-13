#!/usr/bin/env python3
"""Isolated services with distinct tokens: pairing, routing, reconnect and revocation."""
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
from datetime import datetime, timedelta, timezone

from smoke_stdio import Client, FakePlugin, ROOT, until


def stamp(age):
    return (datetime.now(timezone.utc) - age).isoformat().replace('+00:00', 'Z')


def history_logs(directory):
    """Host A's pre-existing logs: one Claude session spanning its 15-day window, one Codex session, one Pi session."""
    claude = directory / 'claude' / '-work-hist'
    claude.mkdir(parents=True)
    with (claude / 'hist-a.jsonl').open('w') as f:
        for uid, age, text in (('h1', timedelta(days=40), 'history too old to share'), ('h2', timedelta(hours=1), 'history shared with peers')):
            f.write(json.dumps({'type': 'assistant', 'uuid': uid, 'sessionId': 'hist-a', 'timestamp': stamp(age), 'cwd': '/work/hist',
                                'message': {'role': 'assistant', 'content': [{'type': 'text', 'text': text}]}}) + '\n')
    pi = directory / 'pi' / '--work-pihist--'
    pi.mkdir(parents=True)
    with (pi / '2026-11-01T10-00-00-000Z_pi-hist.jsonl').open('w') as f:
        f.write(json.dumps({'type': 'session', 'version': 3, 'id': 'pi-hist', 'timestamp': stamp(timedelta(hours=1)), 'cwd': '/work/pihist'}) + '\n')
        f.write(json.dumps({'type': 'message', 'id': 'p1', 'parentId': None, 'timestamp': stamp(timedelta(minutes=50)),
                            'message': {'role': 'assistant', 'content': [{'type': 'text', 'text': 'pi history shared'}], 'stopReason': 'stop'}}) + '\n')
    codex = directory / 'codex' / '2026'
    codex.mkdir(parents=True)
    with (codex / 'rollout-hist.jsonl').open('w') as f:
        f.write(json.dumps({'timestamp': stamp(timedelta(hours=1)), 'type': 'session_meta', 'payload': {'id': 'hist-codex', 'cwd': '/work/hist', 'source': 'cli'}}) + '\n')


def seed_digest(run):
    """Host A's own pending digest, as `rhizome dream` would store it in review mode."""
    now = datetime.now(timezone.utc)
    digests = run / 'memory' / 'digests'
    for directory in (run, run / 'memory', digests):
        directory.mkdir(mode=0o700, exist_ok=True)
        directory.chmod(0o700)
    entry = {'type': 'decision', 'project': 'github.com/acme/app', 'sources': [{'session_id': 'hist-a', 'agent': 'claude'}],
             'expires_at': (now + timedelta(days=7)).isoformat().replace('+00:00', 'Z')}
    digest = {'version': 1, 'id': '00000000000000a1', 'created_at': now.isoformat().replace('+00:00', 'Z'), 'status': 'pending',
              'entries': [dict(entry, text='digest shared with paired hosts'), dict(entry, type='gotcha', text='digest entry left unapproved')]}
    path = digests / '00000000000000a1.json'
    path.write_text(json.dumps(digest))
    path.chmod(0o600)


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

        def row(client, session):
            rows = [s for s in client.tool('list_sessions')['sessions'] if s['identity']['session_id'] == session]
            return rows[0] if rows else None

        try:
            for name in ('a', 'b', 'c'):
                directory = base / name
                directory.mkdir()
                config = directory / 'config.toml'
                run = directory / 'run'
                # Every root is isolated: a default OpenCode root would serve this machine's sessions.
                config.write_text('network.mode = "local"\nruntime_dir = %s\nclaude.projects_dir = %s\ncodex.sessions_dir = %s\n'
                                  'opencode.data_dir = %s\npi.sessions_dir = %s\n'
                                  'gateway.mode = "lan"\ngateway.listen = "127.0.0.1:%d"\n' %
                                  (json.dumps(str(run)), json.dumps(str(directory / 'claude')),
                                   json.dumps(str(directory / 'codex')), json.dumps(str(directory / 'opencode')), json.dumps(str(directory / 'pi')), port()))
                if name == 'a':
                    # Only host A opts in: Claude history for 15 days, Codex history withheld.
                    history_logs(directory)
                    with config.open('a') as f:
                        f.write('gateway.history.enabled = true\nclaude.peer_history.lookback = "15d"\n'
                                'codex.peer_history.enabled = false\n')
                if name in ('a', 'b'):
                    # A and B review digests; C keeps the default (off) and never negotiates them.
                    with config.open('a') as f:
                        f.write('gateway.dream.mode = "review"\n')
                host = {'config': config, 'run': run, 'name': name}
                hosts.append(host)
                start(host)
                client = Client(directory, run=run, env={**{k: '' for k in os.environ if k.startswith('RHIZOME_')}, 'RHIZOME_CONFIG': str(config)})
                client.bind('gateway-' + name, directory)
                clients.append(client)
            a, b, c = hosts
            ca, cb, cc = clients
            # Review flow: only approved entries of A's digest are ever served.
            seed_digest(a['run'])
            assert '00000000000000a1' in command(a, 'dream', 'digest', 'list') and 'pending' in command(a, 'dream', 'digest', 'list')
            command(a, 'dream', 'digest', 'approve', '00000000000000a1', '--entries', '1')
            assert '*[1]' in command(a, 'dream', 'digest', 'show', '00000000000000a1')
            tokens = [(h['run'] / 'token').read_bytes() for h in hosts]
            assert len(set(tokens)) == 3
            code = command(a, 'invite').split()[-1]
            paired = json.loads(command(b, 'join', code))
            command(c, 'join', code, ok=False)
            until(lambda: 'gateway-b' in sessions(ca) and 'gateway-a' in sessions(cb))
            host_a = paired['host_id']
            # A's sessions that ended an hour ago are advertised to B as recent metadata
            # stamped with A's authenticated host; they are never a delivery destination.
            until(lambda: row(cb, 'hist-a') is not None and row(cb, 'hist-codex') is not None and row(cb, 'pi-hist') is not None)
            recent = row(cb, 'hist-a')
            assert recent['status'] == 'recent' and recent['delivery_capable'] is False and recent['host_id'] == host_a, recent
            assert recent['identity']['gateway_host'] == host_a and 'node_id' not in recent['identity'], recent
            assert row(cb, 'hist-codex')['identity']['agent_type'] == 'codex'
            pi_recent = row(cb, 'pi-hist')  # Pi rows travel only because both builds negotiated the Pi subprotocol
            assert pi_recent['status'] == 'recent' and pi_recent['delivery_capable'] is False and pi_recent['host_id'] == host_a and pi_recent['identity']['agent_type'] == 'pi', pi_recent
            local = row(ca, 'hist-a')
            assert local['status'] == 'recent' and 'host_id' not in local, local
            # B pulls A's approved digest over the dream subprotocol, attributed to A's host ID.
            until(lambda: 'digest shared with paired hosts' in command(b, 'dream', 'digest', 'list', '--peers'))
            peers_view = command(b, 'dream', 'digest', 'list', '--peers')
            assert 'UNTRUSTED' in peers_view and 'host=' + host_a in peers_view and 'project github.com/acme/app' in peers_view, peers_view
            assert 'left unapproved' not in peers_view, peers_view
            assert 'no digests received' in command(a, 'dream', 'digest', 'list', '--peers')
            refused = cb.call('tools/call', {'name': 'send', 'arguments': {'session_id': 'hist-a', 'text': 'nobody home'}})
            assert refused.get('isError') and 'recent, not live' in refused['content'][0]['text'], refused
            offer_refused = cb.call('tools/call', {'name': 'send_file', 'arguments': {'session_id': 'hist-a', 'path': str(base / 'b' / 'config.toml')}})
            assert offer_refused.get('isError') and 'live' in offer_refused['content'][0]['text'], offer_refused
            # Explicit binary attachment: offer metadata, immutable staging, acceptance,
            # verified local file, sender receipt, CLI listing and cancellation.
            payload = bytes(range(256)) * 2051
            source = base / 'a' / 'attachment.bin'
            source.write_bytes(payload)
            offer = ca.tool('send_file', {'session_id': 'gateway-b', 'path': str(source)})
            assert offer['state'] == 'offered'
            cb.event('File offered:')
            source.write_bytes(b'changed after offer')
            offers = cb.tool('list_files')
            assert offers[0]['id'] == offer['id'] and 'path' not in offers[0]
            received = cb.tool('accept_file', {'transfer_id': offer['id']})
            assert received['state'] == 'verified' and received['receipt_confirmed']
            assert Path(received['path']).read_bytes() == payload
            assert ca.tool('list_files')[0]['state'] == 'verified'
            assert json.loads(command(b, 'files', 'list', '--session', 'gateway-b'))[0]['id'] == offer['id']
            cb.tool('cancel_file', {'transfer_id': offer['id']})
            assert not Path(received['path']).exists()
            # Opt-in history: B reads A's catalog within A's policy; A cannot read B's.
            until(lambda: 'hist-a' in cb.call('tools/call', {'name': 'search_sessions', 'arguments': {'host_id': host_a}})['content'][0]['text'])
            found = cb.tool('search_sessions', {'host_id': host_a})
            assert sorted(r['session_id'] for r in found['sessions']) == ['hist-a', 'pi-hist'], found
            assert found['sessions'][0]['host_id'] == host_a and found['sessions'][0]['live_status'] == 'not_reported'
            assert found['origin']['host_id'] == host_a and not found['policy']['agents']['codex']['enabled']
            page = cb.tool('read_session_history', {'host_id': host_a, 'agent_type': 'claude', 'session_id': 'hist-a'})
            assert [e['text'] for e in page['events']] == ['history shared with peers'], page
            assert {g['code'] for g in page['coverage']['gaps']} == {'outside_lookback'}
            assert cb.error('read_session_history', {'host_id': host_a, 'agent_type': 'codex', 'session_id': 'hist-codex'})['code'] == 'POLICY_DENIED'
            pi_page = cb.tool('read_session_history', {'host_id': host_a, 'agent_type': 'pi', 'session_id': 'pi-hist'})
            assert [e['text'] for e in pi_page['events'] if e.get('text')] == ['pi history shared'], pi_page
            local = cb.call('tools/call', {'name': 'search_sessions', 'arguments': {}})
            assert 'hist-a' not in local['content'][0]['text'], local  # never replicated into B's catalog
            assert ca.error('search_sessions', {'host_id': json.loads(command(a, 'peers', 'list'))[0]['host_id']})['code'] == 'REMOTE_HISTORY_DISABLED'
            assert 'history shared' not in json.dumps(cb.events) and 'history shared' not in json.dumps(ca.events)
            # A live Pi session on A: the extension bridge is played by a fake, the Rhizome process is real.
            nonce = os.urandom(16).hex()
            pi_proc = Client(base / 'a', run=a['run'], agent='pi', cwd=base / 'a',
                             env={**{k: '' for k in os.environ if k.startswith('RHIZOME_')}, 'RHIZOME_CONFIG': str(a['config']),
                                  'RHIZOME_RUNTIME_DIR': str(a['run']), 'RHIZOME_PI_NONCE': nonce})
            plugin = FakePlugin(a['run'] / ('pi-%d-%s.sock' % (os.getpid(), hashlib.sha256(nonce.encode()).hexdigest()[:12])))
            plugin.send({'type': 'bind', 'nonce': nonce, 'sessionID': 'pi-live', 'directory': str(base / 'a'), 'title': 'Live Pi'})
            until(lambda: (row(cb, 'pi-live') or {}).get('delivery_capable') is True)
            live = row(cb, 'pi-live')
            assert live['status'] == 'live' and live['identity']['agent_type'] == 'pi' and live['identity']['gateway_host'] == host_a, live
            cb.tool('send', {'session_id': 'pi-live', 'text': 'hello live pi'})
            frame = plugin.expect('deliver')
            assert frame['sessionID'] == 'pi-live' and 'hello live pi' in frame['text'] and '<rhizome-peer-event>' in frame['text'], frame
            plugin.send({'type': 'ack', 'id': frame['id']})
            cb.tool('subscribe', {'session_id': 'pi-live'})
            until(lambda: cb.tool('list_sessions')['subscriptions']['pi-live']['state'] == 'following')
            plugin.send({'type': 'input', 'sessionID': 'pi-live', 'kind': 'user'})
            plugin.send({'type': 'text', 'sessionID': 'pi-live', 'messageID': 'm1', 'text': 'pi activity through paired hosts'})
            cb.event('pi activity through paired hosts')
            cb.tool('unsubscribe', {'session_id': 'pi-live'})
            plugin.close()
            pi_proc.close()
            until(lambda: 'pi-live' not in sessions(cb))
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
            before = command(a, 'config')
            gateway_state = {p: p.read_bytes() for p in (a['run'] / 'gateway').rglob('*') if p.is_file()}
            command(a, 'setup', '--network', 'local', '--replace-config')
            assert command(a, 'config') == before
            assert all(p.read_bytes() == data for p, data in gateway_state.items())
            start(a)
            until(lambda: 'gateway-a' in sessions(cb))
            until(lambda: json.loads(command(b, 'peers', 'list'))[0]['connected'])
            cb.tool('send', {'session_id': 'gateway-a', 'text': 'after gateway restart'})
            ca.event('after gateway restart')
            command(c, 'join', command(b, 'invite').split()[-1])
            until(lambda: 'gateway-b' in sessions(cc) and 'gateway-c' in sessions(cb))
            assert 'gateway-a' not in sessions(cc)
            assert 'gateway-c' not in sessions(ca)
            assert 'hist-a' not in sessions(cc)  # recent entries are never forwarded through B
            # C left digest sharing off: its link to B stays up without the dream
            # subprotocol, and digests are neither served to it nor forwarded through B.
            assert 'no digests received' in command(c, 'dream', 'digest', 'list', '--peers')
            # History selection is direct only: C reaches B but never A through B.
            assert cc.error('search_sessions', {'host_id': host_a})['code'] == 'PEER_UNAVAILABLE'
            command(b, 'peers', 'revoke', paired['host_id'])
            until(lambda: 'gateway-a' not in sessions(cb) and 'hist-a' not in sessions(cb))
            until(lambda: 'digest shared' not in command(b, 'dream', 'digest', 'list', '--peers'))  # deleted on revocation
            assert cb.call('tools/call', {'name': 'send', 'arguments': {'session_id': 'gateway-a', 'text': 'must fail'}}).get('isError')
            assert [(h['run'] / 'token').read_bytes() for h in hosts] == tokens
            print('gateway smoke: PASS (pairing, distinct tokens, bidirectional routing, file transfers, opt-in history, recent-only advertisements, reviewed dream digests, Pi over its subprotocol, activity, restart, non-transitivity, revocation)')
        finally:
            for client in clients:
                client.close()
            for process in processes:
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=20)


if __name__ == '__main__':
    main()
