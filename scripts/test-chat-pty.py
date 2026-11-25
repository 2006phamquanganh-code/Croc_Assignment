#!/usr/bin/env python3
"""Exercise real croc chat processes using POSIX PTYs and a local relay.

Usage: python3 scripts/test-chat-pty.py --binary /tmp/croc-chat
No invitation or transcript is printed. Requires Linux or macOS.
"""
import argparse
import fcntl
import os
import pty
import re
import select
import socket
import struct
import subprocess
import termios
import time


class Terminal:
    def __init__(self, binary, args, env):
        self.master, self.slave = pty.openpty()
        fcntl.ioctl(self.slave, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))
        self.original = termios.tcgetattr(self.slave)
        self.output = bytearray()
        self.process = subprocess.Popen([binary, *args], stdin=self.slave,
                                        stdout=self.slave, stderr=self.slave, env=env)

    def until(self, text, timeout=10):
        needle = text.encode()
        end = time.monotonic() + timeout
        while needle not in self.output:
            left = end - time.monotonic()
            if left <= 0:
                raise AssertionError(f"Timed out waiting for {text!r}; exit={self.process.poll()}")
            readable, _, _ = select.select([self.master], [], [], min(left, 0.1))
            if readable:
                try:
                    data = os.read(self.master, 65536)
                except OSError as exc:
                    raise AssertionError("Terminal closed before expected output") from exc
                if not data:
                    raise AssertionError("Terminal reached EOF before expected output")
                self.output.extend(data)
        return bytes(self.output)

    def send(self, text):
        os.write(self.master, text.encode())

    def finished(self):
        code = self.process.wait(timeout=5)
        assert code == 0, f"Chat exited with status {code}"
        assert termios.tcgetattr(self.slave) == self.original, "Terminal mode was not restored"
        while select.select([self.master], [], [], 0)[0]:
            try:
                data = os.read(self.master, 65536)
            except OSError:
                break
            if not data:
                break
            self.output.extend(data)

    def close(self):
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=5)
        os.close(self.master)
        os.close(self.slave)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--public", action="store_true", help="use the public relay pool for a small smoke test")
    args = parser.parse_args()
    binary = os.path.abspath(args.binary)
    env = dict(os.environ)
    for key in list(env):
        if key.startswith("CROC_") or key in ("HTTP_PROXY", "HTTPS_PROXY", "SOCKS5_PROXY"):
            env.pop(key)
    env["CROC_NO_UPDATE_CHECK"] = "1"
    held = []
    for _ in range(2):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        held.append(sock)
    ports = [sock.getsockname()[1] for sock in held]
    for sock in held:
        sock.close()
    relay = None if args.public else subprocess.Popen([binary, "--pass", "chat-test", "relay", "--host", "127.0.0.1",
                              "--ports", ",".join(map(str, ports))],
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env)
    terminals = []
    try:
        deadline = time.monotonic() + 5
        while not args.public:
            try:
                with socket.create_connection(("127.0.0.1", ports[0]), timeout=0.1):
                    break
            except OSError:
                if time.monotonic() > deadline or relay.poll() is not None:
                    raise AssertionError("Local relay failed to start")
                time.sleep(0.05)
        flags = [] if args.public else ["--relay", f"127.0.0.1:{ports[0]}", "--pass", "chat-test"]
        host = Terminal(binary, [*flags, "chat"], env)
        terminals.append(host)
        output = host.until("Waiting for authentication")
        code = re.search(rb"Chat invitation: ([a-z-]+)", output).group(1).decode()
        join = Terminal(binary, [*flags, "chat", "--join"], env)
        terminals.append(join)
        join.until("Enter chat invitation (hidden):")
        join.send(code + "\r")
        host.until("Secure chat connected")
        join.until("Secure chat connected")
        assert code.encode() not in join.output, "Join prompt echoed invitation"
        host.send("Chào B 👋\r")
        join.until("Peer: Chào B 👋")
        host.until("[1] received by peer")
        host.send("bản nháp")
        join.send("Chào A\r")
        host.until("Peer: Chào A")
        join.until("[1] received by peer")
        host.send(" vẫn còn\r")
        join.until("Peer: bản nháp vẫn còn")
        host.until("[2] received by peer")
        join.send("/clear\r")
        join.until("\x1b[2J\x1b[H")
        host.send("/quit\rmust-not-send-after-quit\r")
        host.finished()
        join.finished()
        assert b"must-not-send-after-quit" not in host.output, "Processed pasted text after /quit"
        assert b"must-not-send-after-quit" not in join.output, "Sent pasted text after /quit"
        # A host waiting for a peer must also restore terminal state on Ctrl+C.
        waiting = Terminal(binary, [*flags, "chat"], env)
        terminals.append(waiting)
        waiting.until("Waiting for authentication")
        waiting.send("\x03")
        waiting.finished()
        print("PASS: hidden invitation, Vietnamese/emoji, two-way ACK, draft preservation, clear, quit, Ctrl+C, terminal restoration")
    finally:
        for terminal in terminals:
            terminal.close()
        if relay is not None:
            relay.terminate()
            try:
                relay.wait(timeout=5)
            except subprocess.TimeoutExpired:
                relay.kill()
                relay.wait(timeout=5)


if __name__ == "__main__":
    main()
