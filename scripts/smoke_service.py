#!/usr/bin/env python3
"""Real observer + MCP process coexistence, discovery, handoff, restart and
historical catalog checks (search_sessions / read_session_history)."""
import json
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
import sqlite3
import subprocess
import tempfile
from smoke_stdio import Client, ROOT, until


def append(path, uid, text, role='assistant'):
    with path.open('a') as f:
        f.write(json.dumps({'type': role, 'uuid': uid, 'sessionId': 'observed',
                           'cwd': str(path.parent), 'message': {'content': text}}) + '\n')


def historical(path, session, ts, uid, text, role='assistant'):
    """A timestamped record of a session that ended before the observer existed."""
    content = [{'type': 'text', 'text': text}] if role == 'assistant' else text
    with path.open('a') as f:
        f.write(json.dumps({'type': role, 'uuid': uid, 'sessionId': session, 'timestamp': ts,
                           'cwd': '/work/past', 'message': {'role': role, 'content': content}}) + '\n')


def history_checks(client, running, past_name=('Past work', 'native')):
    """Search and read the pre-existing session; the agent process never existed."""
    opencode_checks(client)
    found = client.tool('search_sessions', {'project_path': '/work/past', 'agent_type': 'claude'})
    catalog = found['catalog']
    assert catalog['service_running'] is running, catalog
    assert catalog['state'] == ('ready' if running else 'stale'), catalog
    assert catalog['coverage_complete'] is running, catalog
    assert [s['session_id'] for s in found['sessions']] == ['past'], found
    past = found['sessions'][0]
    assert (past['session_name'], past['name_source']) == past_name, past
    assert past['live_status'] == 'not_discovered' and past['delivery_capable'] is False, past
    assert past['history_availability'] == 'available', past
    assert past['started_at'] == '2026-09-01T10:00:00Z' and past['last_activity_at'] == '2026-09-01T10:05:00Z', past
    assert found['next_cursor'] is None
    page = client.tool('read_session_history', {'session_id': 'past', 'agent_type': 'claude', 'limit': 1})
    assert [e['text'] for e in page['events']] == ['historical answer'], page
    assert page['coverage'] == {'state': 'complete', 'gaps': [], 'skipped_records': 0,
                                'content_policy': 'assistant_activity_only'}, page
    assert page['next_cursor'], page
    rest = client.tool('read_session_history', {'session_id': 'past', 'agent_type': 'claude', 'limit': 1,
                                                'cursor': page['next_cursor']})
    assert [e['kind'] for e in rest['events']] == ['turn_completed'], rest
    assert rest['next_cursor'] is None and rest['snapshot']['id'] == page['snapshot']['id'], rest
    replay = client.error('read_session_history', {'session_id': 'past', 'agent_type': 'claude', 'limit': 1,
                                                   'cursor': page['next_cursor']})
    assert replay['code'] in ('INVALID_CURSOR', 'CURSOR_EXPIRED'), replay
    assert client.error('search_sessions', {'limit': 0})['code'] == 'INVALID_ARGUMENT'
    assert client.error('read_session_history', {'session_id': 'nope', 'agent_type': 'claude'})['code'] == 'SESSION_NOT_FOUND'
    codex = client.tool('search_sessions', {'agent_type': 'codex'})['sessions']
    assert [s['session_id'] for s in codex] == ['11111111-2222-3333-4444-555555555555'], codex
    assert codex[0]['name_source'] == 'fallback' and codex[0]['project_path'] == '/work/codex', codex
    codex_page = client.tool('read_session_history', {'session_id': codex[0]['session_id'], 'agent_type': 'codex'})
    assert [e['text'] for e in codex_page['events']] == ['codex historical answer', 'Tool started: shell',
                                                        'Tool completed: shell'], codex_page


def opencode_database(root):
    """An OpenCode 2.x database written before the observer existed (WAL, like the real one)."""
    root.mkdir(parents=True)
    db = sqlite3.connect(str(root / 'opencode.db'))
    db.execute('PRAGMA journal_mode=wal')
    db.execute('CREATE TABLE session_v2 (id text PRIMARY KEY, parent_id text, directory text NOT NULL, title text, time_created integer NOT NULL, time_updated integer NOT NULL)')
    db.execute('CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL, time_created integer NOT NULL, data text NOT NULL)')
    db.execute("INSERT INTO session_v2 VALUES ('ses_smoke', NULL, '/work/opencode', 'OpenCode work', 1788256800000, 1788257000000)")
    db.execute("INSERT INTO session_v2 VALUES ('ses_child', 'ses_smoke', '/work/opencode', 'child', 1788256900000, 1788257000000)")
    rows = [
        ('user', 1788256801000, {'text': 'opencode prompt secret'}),
        ('assistant', 1788256802000, {'content': [
            {'type': 'reasoning', 'text': 'hidden reasoning'},
            {'type': 'text', 'text': 'opencode historical answer'},
            {'type': 'tool', 'name': 'read', 'state': {'status': 'completed', 'input': {'k': 'secret input'}},
             'time': {'created': 1788256803000, 'completed': 1788256804000}}]}),
        ('idle', 1788256805000, {'outcome': 'succeeded'}),
    ]
    for seq, (kind, ts, data) in enumerate(rows, 1):
        db.execute('INSERT INTO session_message VALUES (?, ?, ?, ?, ?, ?)', ('m%d' % seq, 'ses_smoke', kind, seq, ts, json.dumps(data)))
    db.execute("INSERT INTO session_message VALUES ('c1', 'ses_child', 'assistant', 1, 1788256900000, ?)",
               (json.dumps({'content': [{'type': 'text', 'text': 'child secret'}]}),))
    db.commit()
    db.close()


def opencode_checks(client):
    found = client.tool('search_sessions', {'agent_type': 'opencode'})['sessions']
    assert [s['session_id'] for s in found] == ['ses_smoke'], found
    assert found[0]['project_path'] == '/work/opencode' and found[0]['session_name'] == 'OpenCode work' \
        and found[0]['name_source'] == 'native' and found[0]['history_availability'] == 'available', found
    page = client.tool('read_session_history', {'session_id': 'ses_smoke', 'agent_type': 'opencode'})
    assert [e['kind'] for e in page['events']] == ['assistant_text', 'tool_summary', 'tool_summary', 'turn_completed'], page
    assert [e['text'] for e in page['events'] if e['text']] == ['opencode historical answer', 'Tool started: read', 'Tool completed: read'], page
    blob = json.dumps(page)
    for secret in ('prompt secret', 'hidden reasoning', 'secret input', 'child secret'):
        assert secret not in blob, secret
    assert page['coverage']['state'] == 'complete', page


def recent_checks(client, running):
    """The ended session is advertised as recent (never live) only while the service runs."""
    state = client.tool('list_sessions')
    rows = [s for s in state['sessions'] if s['identity']['session_id'] == 'ended']
    status = state['recent_sessions']
    if not running:
        assert status['state'] == 'unavailable' and status['reason'] == 'service_stopped', status
        assert not rows, rows
        return
    assert status['state'] == 'enabled' and status['window'] == '24h0m0s' and status['advertised'] == 1, status
    assert len(rows) == 1, state
    row = rows[0]
    assert row['status'] == 'recent' and row['delivery_capable'] is False and 'host_id' not in row, row
    assert row['last_activity_at'] and row['recent_until'] and 'node_id' not in row['identity'], row
    assert row['identity']['session_name'] == 'Ended work' and row['identity']['display_name'] == 'ended', row
    # Older than the window: historical only, never advertised.
    assert not [s for s in state['sessions'] if s['identity']['session_id'] == 'past'], state
    refused = client.call('tools/call', {'name': 'send', 'arguments': {'session_id': 'ended', 'text': 'nobody home'}})
    assert refused.get('isError') and 'recent, not live' in refused['content'][0]['text'], refused


def catalog_ready(client):
    """True once the observer has flushed a complete catalog (written on its first tick)."""
    result = client.call('tools/call', {'name': 'search_sessions', 'arguments': {'session_id': 'past'}})
    return not result.get('isError') and json.loads(result['content'][0]['text'])['catalog']['state'] == 'ready'


def main():
    with tempfile.TemporaryDirectory() as tmp, tempfile.TemporaryFile() as log:
        directory = Path(tmp)
        roots = directory / 'logs'
        roots.mkdir()
        transcript = roots / 'observed.jsonl'
        processes, clients = [], []
        # MCP and the observer share runtime and log roots through one TOML file.
        config = directory / 'config.toml'
        config.write_text('runtime_dir = %s\n[claude]\nprojects_dir = %s\n[codex]\nsessions_dir = %s\n[opencode]\ndata_dir = %s\n'
                          % (json.dumps(str(directory / 'run')), json.dumps(str(roots)), json.dumps(str(directory / 'codex')),
                             json.dumps(str(directory / 'opencode'))))
        opencode_database(directory / 'opencode')
        env = {'RHIZOME_CONFIG': str(config)}
        # Logs written before the service ever ran, including a native title.
        past = roots / 'project' / 'past.jsonl'
        past.parent.mkdir()
        historical(past, 'past', '2026-09-01T10:00:00Z', 'p1', 'historical prompt', 'user')
        with past.open('a') as f:
            f.write(json.dumps({'type': 'custom-title', 'customTitle': 'Past work', 'sessionId': 'past'}) + '\n')
        historical(past, 'past', '2026-09-01T10:04:00Z', 'p2', 'historical answer')
        with past.open('a') as f:
            f.write(json.dumps({'type': 'assistant', 'uuid': 'p3', 'sessionId': 'past', 'timestamp': '2026-09-01T10:05:00Z',
                                'cwd': '/work/past', 'message': {'role': 'assistant', 'content': [{'type': 'thinking', 'thinking': 'hidden'}],
                                                                 'stop_reason': 'end_turn'}}) + '\n')
        # A session that ended an hour ago: inside the default 24-hour recent window.
        ended = roots / 'project' / 'ended.jsonl'
        recent_ts = (datetime.now(timezone.utc) - timedelta(hours=1)).isoformat().replace('+00:00', 'Z')
        with ended.open('a') as f:
            f.write(json.dumps({'type': 'user', 'uuid': 'e1', 'sessionId': 'ended', 'timestamp': recent_ts,
                                'cwd': '/work/ended', 'message': {'role': 'user', 'content': 'recent prompt'}}) + '\n')
            f.write(json.dumps({'type': 'custom-title', 'customTitle': 'Ended work', 'sessionId': 'ended'}) + '\n')
        rollout = directory / 'codex' / '2026' / '09' / '02' / 'rollout.jsonl'
        rollout.parent.mkdir(parents=True)
        with rollout.open('w') as f:
            for record in [
                {'timestamp': '2026-09-02T00:00:00Z', 'type': 'session_meta', 'payload': {'id': '11111111-2222-3333-4444-555555555555', 'cwd': '/work/codex', 'source': 'cli'}},
                {'timestamp': '2026-09-02T00:00:01Z', 'type': 'response_item', 'payload': {'type': 'message', 'id': 'u', 'role': 'user', 'content': [{'type': 'input_text', 'text': 'codex prompt'}]}},
                {'timestamp': '2026-09-02T00:00:02Z', 'type': 'response_item', 'payload': {'type': 'message', 'id': 'a', 'role': 'assistant', 'content': [{'type': 'output_text', 'text': 'codex historical answer'}]}},
                {'timestamp': '2026-09-02T00:00:03Z', 'type': 'response_item', 'payload': {'type': 'function_call', 'id': 'f', 'call_id': 'c', 'name': 'shell', 'arguments': 'secret args'}},
                {'timestamp': '2026-09-02T00:00:04Z', 'type': 'response_item', 'payload': {'type': 'function_call_output', 'call_id': 'c', 'output': 'secret output'}},
            ]:
                f.write(json.dumps(record) + '\n')
        command = [str(ROOT / 'rhizome'), 'service', 'run', '--config', str(config)]
        def start():
            p = subprocess.Popen(command, stdout=log, stderr=log)
            processes.append(p)
            until(lambda: (directory / 'run' / 'observer-health.json').exists())
            assert p.poll() is None
            return p
        try:
            subscriber = Client(directory, env=env)
            clients.append(subscriber)
            subscriber.bind('subscriber', directory)
            # No catalog yet: an actionable error, not an empty result.
            missing = subscriber.error('search_sessions', {})
            assert missing['code'] == 'CATALOG_UNAVAILABLE' and 'rhizome service' in missing['message'], missing
            observer = start()
            until(lambda: catalog_ready(subscriber))
            history_checks(subscriber, running=True)
            health = json.loads((directory / 'run' / 'observer-health.json').read_text())
            assert health['catalog']['state'] == 'ready' and health['catalog']['sessions'] == 4, health
            assert 'historical' not in json.dumps(health) and '/work/past' not in json.dumps(health), health
            # Historical sessions are neither peers nor publishers; a recently ended one is
            # advertised as recent metadata that cannot receive messages.
            assert not [s for s in subscriber.tool('list_sessions')['sessions'] if s['identity']['session_id'] == 'past']
            recent_checks(subscriber, running=True)
            append(transcript, 'initial', 'initial')
            def session():
                return [s for s in subscriber.tool('list_sessions')['sessions']
                        if s['identity']['session_id'] == 'observed']
            until(lambda: len(session()) == 1)
            assert not session()[0]['delivery_capable']
            # Another local session names the observe-only session; its observer
            # publisher re-reads the override and re-advertises it.
            renamed = subscriber.tool('set_session_name', {'session_id': 'observed', 'name': 'Observed rename'})
            assert renamed == {'session_id': 'observed', 'session_name': 'Observed rename', 'source': 'explicit'}, renamed
            until(lambda: session()[0]['identity'].get('session_name') == 'Observed rename')
            assert 'NOT_FOUND' in subscriber.tool_error('set_session_name', {'session_id': 'nope', 'name': 'x'})
            subscriber.tool('subscribe', {'session_id': 'observed'})
            until(lambda: subscriber.tool('list_sessions')['subscriptions']['observed']['state'] == 'following')
            append(transcript, 'passive', 'passive-marker')
            subscriber.event('passive-marker')
            direct = Client(directory, env=env)
            clients.append(direct)
            direct.bind('observed', roots)
            until(lambda: len(session()) == 1 and session()[0]['delivery_capable'])
            subscriber.event('Collection owner changed')
            append(transcript, 'integrated', 'integrated-marker')
            subscriber.event('integrated-marker')
            subscriber.tool('send', {'session_id': 'observed', 'text': 'delivery-marker'})
            direct.event('delivery-marker')
            direct.close()
            clients.remove(direct)
            until(lambda: len(session()) == 1 and not session()[0]['delivery_capable'])
            subscriber.event('Collection owner changed')
            append(transcript, 'boundary', 'handoff boundary', 'user')
            subscriber.event('Direct integration ended')
            append(transcript, 'resume-human', 'continue', 'user')
            append(transcript, 'resumed', 'resumed-marker')
            subscriber.event('resumed-marker')
            # A not-live catalog session is renamed by exact ID from another session.
            renamed = subscriber.tool('set_session_name', {'session_id': 'past', 'name': 'Renamed past'})
            assert renamed['session_name'] == 'Renamed past' and renamed['source'] == 'explicit', renamed
            observer.terminate()
            assert observer.wait(timeout=15) == 0
            processes.remove(observer)
            # A stopped service serves a visibly stale catalog; reads still work, but
            # recent-only presence is withdrawn rather than fabricated.
            history_checks(subscriber, running=False, past_name=('Renamed past', 'explicit'))
            recent_checks(subscriber, running=False)
            start()
            until(lambda: catalog_ready(subscriber))
            history_checks(subscriber, running=True, past_name=('Renamed past', 'explicit'))
            recent_checks(subscriber, running=True)
            cleared = subscriber.tool('set_session_name', {'session_id': 'past', 'name': ''})
            assert cleared['session_name'] == 'Past work' and cleared['source'] == 'native', cleared
            append(transcript, 'restart-human', 'continue again', 'user')
            until(lambda: len(session()) == 1)
            # The override survives the restart and reaches the new publisher.
            until(lambda: session()[0]['identity'].get('session_name') == 'Observed rename')
            # A new observer stream starts live, without replaying old log content.
            subscriber.event('Collection owner changed')
            append(transcript, 'restart', 'restart-marker')
            subscriber.event('restart-marker')
            # Drain transport output via a request and assert no previously consumed
            # marker reappeared after either ownership transition.
            subscriber.tool('list_sessions')
            for message in subscriber.events:
                content = message.get('params', {}).get('content', '')
                assert not any(marker in content for marker in
                               ['passive-marker', 'integrated-marker', 'resumed-marker', 'historical']), content
            # Indexing continues through native watches for live-written logs.
            until(lambda: any(s['session_id'] == 'observed' and s['live_status'] == 'recent'
                              for s in subscriber.tool('search_sessions', {})['sessions']))
        finally:
            for client in clients:
                client.close()
            for process in processes:
                process.terminate()
                process.wait(timeout=15)
    print('PASS: observer discovery, direct handoff/delivery, passive takeover, restart without duplicate replay, '
          'historical catalog search/read, stale catalog while stopped, renaming other local sessions, recent-only advertisement')


if __name__ == '__main__':
    main()
