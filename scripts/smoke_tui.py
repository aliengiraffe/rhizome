#!/usr/bin/env python3
"""Isolated Unix PTY checks; never installs or controls a real user service."""
import fcntl
import http.server
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import tempfile
import termios
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "rhizome"


class Terminal:
    def __init__(self, args, env, width=80, height=24):
        self.master, self.slave = pty.openpty()
        self.before = termios.tcgetattr(self.slave)
        self.resize(width, height)
        self.process = subprocess.Popen([str(BINARY), *args], stdin=self.slave,
                                        stdout=self.slave, stderr=self.slave,
                                        env=env, start_new_session=True)
        self.output = b""
        self.mark = 0

    def resize(self, width, height):
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
        if hasattr(self, "process"):
            self.process.send_signal(signal.SIGWINCH)

    def read(self):
        if select.select([self.master], [], [], 0.05)[0]:
            self.output += os.read(self.master, 65536)

    def expect(self, text):
        deadline = time.monotonic() + 5
        while text.encode() not in self.output[self.mark:]:
            self.read()
            if time.monotonic() > deadline:
                raise AssertionError((text, self.output[self.mark:]))

    def send(self, keys):
        self.mark = len(self.output)
        os.write(self.master, keys)

    def finish(self, code=0):
        deadline = time.monotonic() + 5
        while self.process.poll() is None:
            self.read()
            if time.monotonic() > deadline:
                self.process.kill()
                raise AssertionError("TUI did not stop")
        self.read()
        assert self.process.returncode == code, self.output
        after = termios.tcgetattr(self.slave)
        # BSD sets PENDIN when returning to canonical mode (retype pending input).
        # It is kernel bookkeeping, not a terminal mode left enabled by the UI.
        after[3] &= ~getattr(termios, "PENDIN", 0)
        self.before[3] &= ~getattr(termios, "PENDIN", 0)
        assert after == self.before, ("terminal state not restored", self.before, after)
        assert b"\x1b[?1049l" in self.output, "alternate screen not restored"
        os.close(self.master)
        os.close(self.slave)


def main():
    with tempfile.TemporaryDirectory(prefix="rhizome-tui-") as directory:
        home = Path(directory)
        env = {k: v for k, v in os.environ.items()
               if not k.startswith("RHIZOME_") and k not in ("CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME")}
        env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / "config"), TERM="xterm-256color", NO_COLOR="1")
        config = home / ".rhizome" / "config.toml"
        ui = Terminal(["setup", "--agents", ","], env)
        ui.expect("Network mode")
        ui.send(b"\r"); ui.expect("Hostname override")
        ui.send(b"staged\r"); ui.expect("Install verified updates automatically")
        ui.send(b"\r"); ui.expect("Customize directories")
        ui.send(b"\x1b[Z"); ui.expect("Install verified updates")
        ui.send(b"\x1b[Z"); ui.expect("staged")
        ui.resize(30, 10)
        ui.send(b"\x03"); ui.finish(1)
        assert not config.exists(), "cancel created configuration"
        assert not list(home.rglob("SKILL.md")), "cancel installed skills"

        ui = Terminal(["setup", "--agents", ","], env)
        ui.expect("Network mode")
        for expected in ("Hostname override", "Install verified updates automatically", "Customize directories", "Install and start", "Enter applies"):
            ui.send(b"\r"); ui.expect(expected)
        ui.send(b"\r"); ui.expect("Setup completed")
        ui.send(b"\r"); ui.finish()
        assert config.stat().st_mode & 0o777 == 0o600

        for index, args in enumerate((["service"], ["service", "status", "--tui"])):
            ui = Terminal(args, env)
            ui.expect("Observer running: false")
            if len(args) == 1:
                ui.send(b"x"); ui.expect("Confirm stop?")
                ui.send(b"n"); ui.expect("Observer running: false")
            else:
                ui.send(b"i")  # read-only ignores mutation keys
            health = home / ".rhizome" / "run" / "observer-health.json"
            health.parent.mkdir(parents=True, exist_ok=True)
            health.write_text(json.dumps({"observed_sessions": index + 42}))
            ui.send(b"r"); ui.expect(f"observed_sessions: {index + 42}")
            ui.resize(24, 8)
            ui.send(b"q"); ui.finish()
        assert not (home / "Library" / "LaunchAgents").exists()
        assert not (home / "config" / "systemd").exists()

        ui = Terminal(["peers", "list", "--tui"], env)
        ui.expect("gateway service unavailable")
        ui.send(b"q"); ui.finish(1)

        class Handler(http.server.BaseHTTPRequestHandler):
            result = []
            block = False
            started = threading.Event()
            release = threading.Event()

            def do_POST(self):
                command = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                assert command["action"] == "peers"
                if self.block:
                    self.started.set()
                    self.release.wait(5)
                self.send_response(200)
                self.end_headers()
                try:
                    self.wfile.write(json.dumps(self.result).encode())
                except BrokenPipeError:
                    pass

            def log_message(self, *_):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        control = home / ".rhizome" / "run" / "gateway" / "control.json"
        control.parent.mkdir(parents=True, mode=0o700)
        control.write_text(json.dumps({"Endpoint": f"127.0.0.1:{server.server_port}", "Secret": "a" * 64}))
        control.chmod(0o600)
        try:
            result = subprocess.run([str(BINARY), "peers", "list"], env=env, capture_output=True, timeout=5)
            assert result.returncode == 0 and result.stdout == b"[]\n", result
            ui = Terminal(["peers", "list", "--tui"], env)
            ui.expect("No paired hosts")
            Handler.result = [{"host_id": "b" * 64, "endpoint": "label\u001b[2J", "connected": True, "pending": False}]
            ui.send(b"r"); ui.expect("b" * 64)
            assert b"label\x1b[2J" not in ui.output
            ui.resize(24, 8)
            ui.send(b"q"); ui.finish()
            Handler.block = True
            ui = Terminal(["peers", "list", "--tui"], env)
            assert Handler.started.wait(5)
            ui.send(b"\x03"); ui.finish()
        finally:
            Handler.release.set()
            server.shutdown()
            server.server_close()
            thread.join()

        for args in (["service", "status", "--tui"], ["peers", "list", "--tui"]):
            result = subprocess.run([str(BINARY), *args], env=env, capture_output=True, timeout=5)
            assert result.returncode != 0 and b"requires terminal" in result.stderr
            assert b"\x1b" not in result.stdout + result.stderr
        result = subprocess.run([str(BINARY), "setup", "--plain", "--agents", ",", "--network", "local"],
                                env=env, capture_output=True, timeout=5)
        assert result.returncode == 0 and b"\x1b" not in result.stdout + result.stderr
    print("TUI PTY checks passed: setup, service, peers, cancellation, resize, terminal restoration, JSON, redirection")


if __name__ == "__main__":
    main()
