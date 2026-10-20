#!/usr/bin/env python3
"""Opt-in live OpenCode smoke: a disposable standalone OpenCode server with the
Rhizome plugin and MCP entry in an isolated config, one session, a Claude-protocol
peer, collected activity and a pushed delivery. Uses your configured provider
credentials and runs real model turns; sessions are deleted afterwards."""
import argparse
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'scripts'))
from smoke_stdio import Client, until  # noqa: E402

DENY = ['shell', 'read', 'edit', 'glob', 'grep', 'webfetch', 'websearch', 'subagent', 'question', 'skill',
        'external_directory', 'opencode_list_mcp_resources', 'opencode_read_mcp_resource',
        'opencode_worktree_create', 'opencode_worktree_remove', 'opencode_session_move', 'opencode_session_rename']

def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]

def wait_for(check, seconds):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(.5)
    raise AssertionError('timed out after %ss' % seconds)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--live', action='store_true', help='allow disposable sessions and model turns')
    parser.add_argument('--model', help='provider/model to use, e.g. opencode-go/deepseek-v4.1-flash')
    args = parser.parse_args()
    if not args.live or not args.model:
        parser.error('refusing to run without --live and --model')
    binary = shutil.which('opencode')
    assert binary, 'opencode not on PATH'
    out = subprocess.run([binary, '--version'], capture_output=True, text=True, timeout=30).stdout.split()
    assert out, 'opencode --version printed nothing'
    version = out[-1]
    assert version.lstrip('v').startswith('2.'), 'OpenCode 2.x required, found ' + version
    try:
        turn_timeout = int(os.environ.get('RZOC_T', '240'))
    except ValueError:
        print('RZOC_T is not an integer; using 240', file=sys.stderr)
        turn_timeout = 240
    tmp = Path(tempfile.mkdtemp(prefix='rzoc-', dir='/tmp'))
    server = server_log = peer = None
    sessions = []
    url = env = project = password = None
    try:
        run, project, config = tmp / 'run', tmp / 'project', tmp / 'config' / 'opencode'
        for d in (run, project, config / 'plugins'):
            d.mkdir(parents=True)
        run.chmod(0o700)
        project = project.resolve()
        subprocess.run(['git', 'init', '-q', str(project)], check=True)
        # An empty commit gives the repo its own OpenCode project; a commitless repo
        # would share the "global" project with the user's real sessions.
        # Throwaway repo: never sign, whatever the user's global git config says.
        subprocess.run(['git', '-c', 'user.name=rz', '-c', 'user.email=rz@local', '-c', 'commit.gpgsign=false',
                        'commit', '--allow-empty', '-m', 'init', '-q'], cwd=project, check=True)
        shutil.copy(ROOT / 'plugins' / 'opencode' / 'rhizome.js', config / 'plugins' / 'rhizome.js')
        (config / 'opencode.json').write_text(json.dumps({
            '$schema': 'https://opencode.ai/config.json',
            'model': args.model,
            'mcp': {'servers': {'rhizome': {'type': 'local', 'command': [str(ROOT / 'rhizome'), 'mcp', '--agent', 'opencode'],
                                            'environment': {'RHIZOME_RUNTIME_DIR': str(run)}}}},
            'permissions': [{'action': a, 'resource': '*', 'effect': 'deny'} for a in DENY],
        }))
        env = {**os.environ, 'XDG_CONFIG_HOME': str(tmp / 'config'), 'XDG_STATE_HOME': str(tmp / 'state'),
               'XDG_CACHE_HOME': str(tmp / 'cache'), 'RHIZOME_RUNTIME_DIR': str(run)}
        port = free_port()
        server_log = (tmp / 'serve.log').open('w')
        server = subprocess.Popen([binary, 'serve', '--hostname', '127.0.0.1', '--port', str(port)], cwd=project,
                                  env=env, stdout=server_log, stderr=subprocess.STDOUT)
        url = 'http://127.0.0.1:%d' % port
        password = wait_for(lambda: next((l.split()[-1] for l in (tmp / 'serve.log').read_text().splitlines()
                                          if l.startswith('server password')), None), 30)
        env['OPENCODE_PASSWORD'] = password

        def opencode(*argv, timeout=turn_timeout, check=True):
            done = subprocess.run([binary, *argv, '--server', url], cwd=project, env=env, stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, timeout=timeout)
            for line in done.stdout.splitlines():  # record IDs even when the turn fails
                if '"sessionID"' in line:
                    try:
                        sid = json.loads(line)['sessionID']
                    except (ValueError, KeyError):
                        continue
                    if sid not in sessions:
                        sessions.append(sid)
            if check and done.returncode != 0:
                excerpt = done.stderr.strip()
                if password:  # never echo the server password; scrub before clipping
                    excerpt = excerpt.replace(password, '***')
                excerpt = excerpt[-300:]
                raise AssertionError('opencode %s failed (exit %d): %s' % (argv[0], done.returncode, excerpt))
            return done

        first = opencode('run', '--model', args.model, '--format', 'json', 'Reply with the single word ready.')
        assert sessions, 'no session ID in opencode run output'
        session = sessions[0]
        # Client uses <dir>/run as RHIZOME_RUNTIME_DIR, so tmp shares the real runtime
        # directory (Rhizome refuses symlinked runtime directories).
        peer = Client(tmp)
        peer.bind('claude-peer', tmp)
        wait_for(lambda: any(s['identity']['session_id'] == session and s['identity']['agent_type'] == 'opencode'
                             for s in peer.tool('list_sessions')['sessions']), 60)
        peer.tool('subscribe', {'session_id': session})
        until(lambda: peer.tool('list_sessions')['subscriptions'][session]['state'] == 'following')
        opencode('run', '--session', session, '--model', args.model, '--format', 'json',
                 'Reply with exactly these words: opencode live marker')

        def got_marker():
            peer.call('tools/list', {})
            return any(m.get('method') == 'notifications/claude/channel' and 'opencode live marker' in m['params']['content']
                       for m in peer.events)
        wait_for(got_marker, 120)
        peer.tool('send', {'session_id': session, 'text': 'live-delivery-marker: no action needed'})

        def delivered():
            out = opencode('session', 'export', session, timeout=60, check=False).stdout
            return 'live-delivery-marker' in out and '<rhizome-peer-event>' in out
        wait_for(delivered, 120)
        print('PASS: OpenCode %s live binding, activity to a Claude peer and synthetic delivery' % version)
    finally:
        try:
            if peer:
                peer.close()
        except Exception as error:
            print('cleanup: peer close failed: %s' % error, file=sys.stderr)
        if url and env is not None:
            # Delete only sessions this script recorded, plus listed sessions whose directory is the
            # temp project. Without a directory field, unrecorded sessions are never deleted.
            doomed = list(sessions)
            try:
                listing = subprocess.run([binary, 'session', 'list', '--format', 'json', '--server', url],
                                         cwd=project, env=env, capture_output=True, text=True, timeout=60).stdout
                for item in json.loads(listing):
                    sid = item.get('id')
                    if sid in sessions:
                        continue
                    directory = item.get('directory')
                    if directory is None:
                        print('cleanup: skipped %s (no directory field)' % sid, file=sys.stderr)
                    elif os.path.realpath(directory) == str(project):
                        doomed.append(sid)
                    else:
                        print('cleanup: skipped %s (other directory)' % sid, file=sys.stderr)
            except Exception as error:
                print('cleanup: session list failed: %s' % error, file=sys.stderr)
            for sid in doomed:
                try:
                    done = subprocess.run([binary, 'session', 'delete', sid, '--server', url], cwd=project, env=env,
                                          capture_output=True, text=True, timeout=60)
                    print('cleanup: deleted %s (exit %d)' % (sid, done.returncode), file=sys.stderr)
                except Exception as error:
                    print('cleanup: delete %s failed: %s' % (sid, error), file=sys.stderr)
        try:
            if server:
                server.terminate()
                try:
                    server.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    server.kill()
        except Exception as error:
            print('cleanup: server stop failed: %s' % error, file=sys.stderr)
        try:
            if server_log:
                server_log.close()
        except Exception as error:
            print('cleanup: log close failed: %s' % error, file=sys.stderr)
        if os.environ.get('RZOC_KEEP'):
            print('WARNING: kept %s; it holds the server password and config. Delete it when done.' % tmp,
                  file=sys.stderr)
        else:
            shutil.rmtree(tmp, ignore_errors=True)

if __name__ == '__main__':
    main()
