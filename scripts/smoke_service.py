#!/usr/bin/env python3
"""Real observer + MCP process coexistence, discovery, handoff and restart checks."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
from smoke_stdio import Client, ROOT, until


def append(path, uid, text, role='assistant'):
    with path.open('a') as f:
        f.write(json.dumps({'type': role, 'uuid': uid, 'sessionId': 'observed',
                           'cwd': str(path.parent), 'message': {'content': text}}) + '\n')


def main():
    with tempfile.TemporaryDirectory() as tmp, tempfile.TemporaryFile() as log:
        directory = Path(tmp)
        roots = directory / 'logs'
        roots.mkdir()
        transcript = roots / 'observed.jsonl'
        processes, clients = [], []
        command = [str(ROOT / 'rhizome'), 'service', 'run', '--runtime-dir', str(directory / 'run'),
                   '--claude-root', str(roots), '--codex-root', str(directory / 'codex')]
        def start():
            p = subprocess.Popen(command, stdout=log, stderr=log)
            processes.append(p)
            until(lambda: (directory / 'run' / 'observer-health.json').exists())
            assert p.poll() is None
            return p
        try:
            observer = start()
            subscriber = Client(directory)
            clients.append(subscriber)
            subscriber.bind('subscriber', directory)
            append(transcript, 'initial', 'initial')
            def session():
                return [s for s in subscriber.tool('list_sessions')['sessions']
                        if s['identity']['session_id'] == 'observed']
            until(lambda: len(session()) == 1)
            assert not session()[0]['delivery_capable']
            subscriber.tool('subscribe', {'session_id': 'observed'})
            until(lambda: subscriber.tool('list_sessions')['subscriptions']['observed']['state'] == 'following')
            append(transcript, 'passive', 'passive-marker')
            subscriber.event('passive-marker')
            direct = Client(directory)
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
            observer.terminate()
            assert observer.wait(timeout=15) == 0
            processes.remove(observer)
            start()
            append(transcript, 'restart-human', 'continue again', 'user')
            until(lambda: len(session()) == 1)
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
                               ['passive-marker', 'integrated-marker', 'resumed-marker']), content
        finally:
            for client in clients:
                client.close()
            for process in processes:
                process.terminate()
                process.wait(timeout=15)
    print('PASS: observer discovery, direct handoff/delivery, passive takeover, restart without duplicate replay')


if __name__ == '__main__':
    main()
