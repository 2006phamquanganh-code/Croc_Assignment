package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"github.com/schollz/croc/v11/src/chat"
	"golang.org/x/term"
)

type chatInputResult struct {
	data []byte
	err  error
}

// safeChatText keeps peer content in a text-only rendering path. Protocol checks
// and ACK operate on the original bytes, not this display representation.
func safeChatText(text string) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\u2028' || r == '\u2029':
			b.WriteString("\r\n  ")
		case r == '\t':
			b.WriteString("    ")
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// chatEditor consumes arbitrary read chunking and ignores terminal escape input.
// Bound draft bytes independently of the per-message TEXT validation.
type chatEditor struct {
	draft    []rune
	partial  []byte
	escape   int
	bytes    int
	overflow bool
	stopped  bool
}

func (e *chatEditor) feed(data []byte, submit func(string) bool) (quit bool) {
	e.partial = append(e.partial, data...)
	for len(e.partial) > 0 {
		ch := e.partial[0]
		if e.escape != 0 {
			e.partial = e.partial[1:]
			switch e.escape {
			case 1:
				if ch == '[' || ch == 'O' {
					e.escape = 2
				} else if ch == ']' {
					e.escape = 3
				} else {
					e.escape = 0
				}
			case 2:
				if ch >= 0x40 && ch <= 0x7e {
					e.escape = 0
				}
			case 3:
				if ch == 7 {
					e.escape = 0
				} else if ch == 27 {
					e.escape = 4
				}
			case 4:
				if ch == '\\' {
					e.escape = 0
				} else {
					e.escape = 3
				}
			}
			continue
		}
		switch ch {
		case 3, 4:
			return true
		case 27:
			e.escape = 1
			e.partial = e.partial[1:]
			continue
		case 21:
			e.partial = e.partial[1:]
			clear(e.draft)
			e.draft = nil
			e.bytes = 0
			e.overflow = false
			continue
		case 13, 10:
			e.partial = e.partial[1:]
			if len(e.draft) > 0 && submit(string(e.draft)) {
				clear(e.draft)
				e.draft = nil
				e.bytes = 0
			}
			if e.stopped {
				clear(e.partial)
				e.partial = nil
				return true
			}
			continue
		case 8, 127:
			e.partial = e.partial[1:]
			if len(e.draft) > 0 {
				e.bytes -= utf8.RuneLen(e.draft[len(e.draft)-1])
				e.draft = e.draft[:len(e.draft)-1]
			}
			continue
		}
		if !utf8.FullRune(e.partial) {
			break
		}
		r, n := utf8.DecodeRune(e.partial)
		e.partial = e.partial[n:]
		if r == utf8.RuneError && n == 1 || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if e.bytes+n <= 64<<10 {
			e.draft = append(e.draft, r)
			e.bytes += n
		} else {
			e.overflow = true
		}
	}
	return false
}

func readChatInvitation(ctx context.Context, input <-chan chatInputResult, output io.Writer) (string, error) {
	var editor chatEditor
	defer func() { clear(editor.draft); clear(editor.partial) }()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case in, ok := <-input:
			if !ok || in.err != nil {
				return "", io.EOF
			}
			var secret string
			quit := editor.feed(in.data, func(line string) bool { secret = line; return true })
			clear(in.data)
			if quit {
				fmt.Fprint(output, "\r\n")
				return "", context.Canceled
			}
			if secret != "" {
				fmt.Fprint(output, "\r\n")
				return secret, nil
			}
		}
	}
}

func chatDraftTail(draft []rune, width int) string {
	text := string(draft)
	// A single physical line avoids damaged prompts when peer messages arrive.
	limit := max(1, width-4)
	graphemes := uniseg.NewGraphemes(text)
	var parts []string
	for graphemes.Next() {
		parts = append(parts, graphemes.Str())
	}
	used := 0
	start := len(parts)
	for start > 0 {
		w := uniseg.StringWidth(parts[start-1])
		if used+w > limit {
			break
		}
		used += w
		start--
	}
	return strings.Join(parts[start:], "")
}

func runChatUI(ctx context.Context, s *chat.Session, input <-chan chatInputResult, output io.Writer) error {
	var editor chatEditor
	defer func() { clear(editor.draft); clear(editor.partial); fmt.Fprint(output, "\r\x1b[2K\r\n") }()
	pending := make(map[uint64]bool)
	var history []string
	historyBytes := 0
	defer func() { clear(history) }()
	active := false
	peerTyping := false
	var lastTypingSent time.Time
	localTyping := false
	redraw := func() {
		width, _, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			width = 80
		}
		prefix := "> "
		if peerTyping {
			prefix = "[typing...] > "
		}
		fmt.Fprintf(output, "\r\x1b[2K%s%s", prefix, chatDraftTail(editor.draft, width))
	}
	appendHistory := func(text string) {
		history = append(history, text)
		historyBytes += len(text)
		for len(history) > 500 || historyBytes > 2<<20 {
			historyBytes -= len(history[0])
			history[0] = ""
			history = history[1:]
		}
	}
	line := func(text string) {
		appendHistory(text)
		fmt.Fprintf(output, "\r\x1b[2K%s\r\n", text)
	}
	renderHistory := func(clearScrollback bool) {
		if clearScrollback {
			fmt.Fprint(output, "\x1b[2J\x1b[3J\x1b[H")
		} else {
			fmt.Fprint(output, "\x1b[2J\x1b[H")
		}
		for _, h := range history {
			if h != "" {
				fmt.Fprintf(output, "%s\r\n", h)
			}
		}
	}
	replaceLine := func(needle, replacement string, clearScrollback bool) bool {
		replaced := false
		for i, h := range history {
			if strings.Contains(h, needle) {
				historyBytes -= len(h)
				history[i] = replacement
				historyBytes += len(replacement)
				replaced = true
			}
		}
		if replaced {
			renderHistory(clearScrollback)
		}
		return replaced
	}
	parseTTLCommand := func(raw string) (time.Duration, string, error) {
		rest := strings.TrimSpace(strings.TrimPrefix(raw, "/ttl"))
		fields := strings.SplitN(rest, " ", 2)
		if len(fields) < 2 || strings.TrimSpace(fields[1]) == "" {
			return 0, "", errors.New("usage: /ttl <seconds> <message> (e.g. /ttl 10 secret)")
		}
		durStr := strings.TrimSpace(fields[0])
		var ttl time.Duration
		if secs, err := strconv.Atoi(durStr); err == nil {
			ttl = time.Duration(secs) * time.Second
		} else if d, err := time.ParseDuration(durStr); err == nil {
			ttl = d
		} else {
			return 0, "", errors.New("invalid TTL duration; use seconds like 10 or 30s")
		}
		return ttl, fields[1], nil
	}
	quit := false
	submit := func(text string) bool {
		if editor.overflow {
			line("Draft exceeded 64 KiB; press Ctrl+U to clear it.")
			return false
		}
		if strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "//") {
			if text == "/ttl" || strings.HasPrefix(text, "/ttl ") {
				if !active {
					line("Waiting for the secure connection; draft kept.")
					return false
				}
				ttl, msg, err := parseTTLCommand(text)
				if err != nil {
					line(err.Error())
					return false
				}
				id, err := s.SendTTLText(msg, ttl)
				if err != nil {
					line("TTL message not queued; draft kept: " + safeChatText(err.Error()))
					return false
				}
				if localTyping {
					localTyping = false
					_ = s.SendTyping(false)
				}
				pending[id] = true
				line(fmt.Sprintf("You [%d] [TTL %s]: %s", id, ttl.Truncate(time.Second), safeChatText(msg)))
				return true
			}
			switch text {
			case "/quit":
				quit = true
				editor.stopped = true
				return true
			case "/clear":
				clear(history)
				history = nil
				historyBytes = 0
				fmt.Fprint(output, "\x1b[2J\x1b[H")
				return true
			case "/help":
				line("/help  /status  /ttl <sec> <msg>  /clear  /quit; start with // to send a leading slash")
				return true
			case "/status":
				line("Connection: " + safeChatText(s.TransportStats()))
				return true
			default:
				line("Unknown command. Use /help.")
				return false
			}
		}
		if !active {
			line("Waiting for the secure connection; draft kept.")
			return false
		}
		if strings.HasPrefix(text, "//") {
			text = text[1:]
		}
		id, err := s.SendText(text)
		if err != nil {
			line("Message not queued; draft kept: " + safeChatText(err.Error()))
			return false
		}
		if localTyping {
			localTyping = false
			_ = s.SendTyping(false)
		}
		pending[id] = true
		line(fmt.Sprintf("You [%d]: %s", id, safeChatText(text)))
		return true
	}
	for {
		select {
		case <-ctx.Done():
			s.Close()
			s.Wait()
			return ctx.Err()
		case in, ok := <-input:
			if !ok || in.err != nil {
				s.Close()
				return s.Wait()
			}
			if editor.feed(in.data, submit) {
				quit = true
			}
			clear(in.data)
			if quit {
				s.Close()
				return s.Wait()
			}
			if active {
				if len(editor.draft) > 0 {
					if !localTyping || time.Since(lastTypingSent) >= 2*time.Second {
						localTyping = true
						lastTypingSent = time.Now()
						_ = s.SendTyping(true)
					}
				} else if localTyping {
					localTyping = false
					_ = s.SendTyping(false)
				}
			}
			redraw()
		case e, ok := <-s.Events():
			if !ok {
				return s.Wait()
			}
			switch e.Type {
			case chat.Connected:
				active = true
				line(fmt.Sprintf("Secure chat connected [%s]. /help lists commands; Ctrl+C exits.", safeChatText(s.TransportStats())))
			case chat.MessageReceived:
				peerTyping = false
				if e.TTL > 0 {
					line(fmt.Sprintf("Peer [%d] [TTL %s]: %s", e.ID, e.TTL.Truncate(time.Second), safeChatText(e.Text)))
				} else {
					line("Peer: " + safeChatText(e.Text))
				}
			case chat.MessageExpired:
				needle := fmt.Sprintf("[%d] [TTL ", e.ID)
				expiredLine := fmt.Sprintf("[TTL] Message [%d] expired and wiped.", e.ID)
				if !replaceLine(needle, expiredLine, true) {
					line(expiredLine)
				}
			case chat.PeerTyping:
				peerTyping = e.Typing
			case chat.KeyRotated:
				line(fmt.Sprintf("[Security] Session AES-256-GCM key rotated via HKDF-SHA256 (epoch %d).", e.ID))
			case chat.Sent:
				if pending[e.ID] {
					line(fmt.Sprintf("✓ [%d] waiting for receipt", e.ID))
				}
			case chat.Acknowledged:
				delete(pending, e.ID)
				ackLine := fmt.Sprintf("✓✓ [%d] received by peer", e.ID)
				waitNeedle := fmt.Sprintf("✓ [%d] waiting for receipt", e.ID)
				removed := false
				for i, h := range history {
					if strings.HasPrefix(h, "✓✓ [") || strings.Contains(h, waitNeedle) {
						historyBytes -= len(h)
						history[i] = ""
						removed = true
					}
				}
				appendHistory(ackLine)
				if removed {
					renderHistory(false)
				} else {
					fmt.Fprintf(output, "\r\x1b[2K%s\r\n", ackLine)
				}
			case chat.Uncertain:
				if pending[e.ID] {
					uncertainLine := fmt.Sprintf("? [%d] delivery uncertain", e.ID)
					if !replaceLine(fmt.Sprintf("✓ [%d] waiting for receipt", e.ID), uncertainLine, false) {
						line(uncertainLine)
					}
				}
			case chat.Closed, chat.Failed:
				if len(pending) > 0 {
					line("Unacknowledged messages have uncertain delivery.")
				}
				if e.Err != nil && !errors.Is(e.Err, context.Canceled) {
					line("Chat ended: " + safeChatText(e.Err.Error()))
				} else {
					line("Chat ended.")
				}
				return s.Wait()
			}
			redraw()
		}
	}
}
