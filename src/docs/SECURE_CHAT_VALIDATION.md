# Secure chat validation

Validation date: 25 November 2026. These results apply to the working tree that
adds `croc chat`, not to an already published croc release.

## Confirmed locally

| Check | Result |
| --- | --- |
| `go test ./...` | Passed on Linux amd64, Go 1.27.3 |
| `go test -race ./src/chat ./src/comm` | Passed |
| `go test -race ./src/cli -run Chat` | Passed |
| `go test -tags croc_no_tailcat ./src/chat ./src/cli` | Passed |
| `go vet ./src/chat ./src/cli ./src/comm` | Passed |
| Five-second bounded fuzz runs for record and handshake decoders | Passed |
| Linux normal and `croc_no_tailcat` builds | Passed |
| Windows amd64 CLI test binary cross-compilation | Passed |
| macOS arm64 CLI test binary cross-compilation | Passed |
| Development executables for Windows amd64, macOS arm64 and macOS amd64 | Cross-built; executable formats and SHA-256 checksums verified |
| Two real CLI processes over a local relay with POSIX PTYs | Passed |
| Same POSIX PTY test using the relay-only binary | Passed |
| Two CLI processes on the same Linux host using the public relay pool | Passed |
| Help and runtime-generated shell completion include `chat` | Passed |
| Full chat sessions through local SOCKS5 and HTTP CONNECT proxies | Passed under the race detector |
| Third participant rejected without evicting the original pair | Passed |
| Admission deadline cancels the socket and preserves context error | Passed |

The PTY test covers a hidden invitation prompt, Vietnamese/emoji, bidirectional
receipts, an intact draft while receiving another message, `/clear`, `/quit`,
Ctrl+C while awaiting a peer, discarding text pasted after `/quit`, and
restoration of terminal mode.

Protocol tests cover independent HKDF and AES-GCM vectors, wrong passwords and
purposes, tampered/replayed records, invalid versions and sequences, strict JSON
handshake fields and sizes, a real existing relay, bounded queues, missing PONG,
late and duplicate ACK, expired sessions and cancellation. A gated connection
forces ACK to arrive before the sender's write call returns. Other tests cover
blocked writers and peers that read CLOSE without sending a reply. A debug-log
check verifies invitation, password, room and test message are absent from logs.
Additional regressions verify that an accepted message remains uncertain if
setting the write deadline fails, parent context deadlines retain their error
type, and Windows UTF-16 input and key-repeat decoding are bounded and correct.
These decoder tests run on Linux too; they do not replace a native Windows
console smoke test.

## Outstanding native verification

- Run the interactive console scenario in [TESTING](TESTING.md#secure-chat) on
  Windows, including Unicode, hidden input, cancellation and console mode restoration.
- Execute the macOS PTY scenario natively. Cross-compilation proves buildability,
  not runtime terminal behavior.
- Run the two-machine public-relay scenario on different networks. The completed
  public-relay smoke test used two processes on one host.
- Execute the updated CI workflow on its native runners. The workflow has been
  edited locally; these results do not imply it has run remotely.

The feature remains under validation until these checks have evidence. No
release, tag, commit, push or pull request was created by this implementation.
