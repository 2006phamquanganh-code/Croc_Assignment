# Testing

## Secure chat

Run protocol, relay integration, CLI and deadline tests:

```bash
go test ./src/chat ./src/codephrase ./src/pakekey ./src/comm ./src/cli
go test -race ./src/chat ./src/comm
go test -race ./src/cli -run Chat
go test -tags croc_no_tailcat ./src/chat ./src/cli
go test ./src/chat -run '^$' -fuzz FuzzRecordOpen -fuzztime=10s -parallel=2
go test ./src/chat -run '^$' -fuzz FuzzHandshakeDecode -fuzztime=10s -parallel=2
```

On Linux or macOS, build and exercise two real CLI processes through a local
relay with POSIX pseudo terminals:

```bash
go build -o /tmp/croc-chat .
python3 scripts/test-chat-pty.py --binary /tmp/croc-chat
```

The script checks the hidden invitation prompt, Vietnamese/emoji delivery,
receipts in both directions, a preserved draft while a peer message arrives,
`/clear`, `/quit`, Ctrl+C while waiting, and restoration of terminal mode. It
does not print the invitation or transcript. It needs permission to bind local
TCP sockets and create PTYs. Repeat with a `croc_no_tailcat` build to exercise
relay-only packaging. Neither automated test requires a public relay. To opt into a small public
relay smoke test on the same machine, append `--public` to the script command;
it checks relay compatibility, while a two-network demo still needs two machines.

For native Windows console and a manual macOS check, use three terminals: start
`croc relay --ports 9009,9010`, run `croc --relay 127.0.0.1:9009 chat`, then
`croc --relay 127.0.0.1:9009 chat --join`. Verify hidden input, Unicode,
backspace, Ctrl+U, incoming messages while typing, receipt status, `/quit`,
Ctrl+C, and normal terminal echo after exit. Kill a peer or stop the relay and
confirm pending messages have uncertain delivery and the remaining client exits.
Cross-compilation alone does not verify these native console behaviors.

For a public relay smoke test, run `croc chat` on one machine and `croc chat
--join` on another, preferably on a different network. Exchange a few messages
and exit. Keep repeated load/failure tests on a private local relay. Record the
tested revision, operating systems and results without including secrets.

## Transfer Update Checks

Send- and receive-side release checks are normally cached for 24 hours. Set
`CROC_DO_CHECK=1` to bypass a fresh cache, wait for a release request before the
transfer starts, and report whether a newer release is available:

```bash
CROC_DO_CHECK=1 croc send file.txt
```

The override waits up to two seconds for the request and reports when the current
version is already latest or when the check is unavailable. It does not display
a notice under `--quiet` or turn update-check failures into errors.

Set `CROC_DO_CHECK=0` to skip the check entirely. Croc then makes no release
request, neither reads nor writes `version-check.json`, and shows no update
notice:

```bash
CROC_DO_CHECK=0 croc send file.txt
```

`--no-update-check` and `CROC_NO_UPDATE_CHECK=1` also skip it, and `--remember` on
a regular send or receive keeps that setting. `CROC_DO_CHECK=1` forces a check over
any of them.

`croc update --check` is unaffected.

## Local Reconnect Interruptions

Use three terminals to run a local relay, receiver, and sender. From the repository root, build the binary and start a local relay in the first terminal:

```bash
go build -o ./bin/croc && ./bin/croc relay --ports 9009,9010
```

In the second terminal, start the receiver:

```bash
rm -rf croc-big.bin && ./bin/croc --debug --yes --overwrite --relay 127.0.0.1:9009 test-reconnect-code
```

In the third terminal, start the sender:

```bash
./bin/croc --relay 127.0.0.1:9009 --throttleUpload 512K --no-compress send --code test-reconnect-code --no-local --no-multi /tmp/croc-big.bin
```

While the transfer is running, interrupt one active socket.

For a data-channel drop:

```bash
sudo ss -K dst 127.0.0.1 dport = :9010
```

For a control-channel drop:

```bash
sudo ss -K dst 127.0.0.1 dport = :9009
```

If no connection is killed, inspect the active croc sockets and adjust the port:

```bash
sudo ss -tnp | grep croc
```
