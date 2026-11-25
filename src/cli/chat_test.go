package cli

import (
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/schollz/croc/v11/internal/cli"
)

func TestChatCommandAndOptions(t *testing.T) {
	for _, test := range []struct {
		args  []string
		valid bool
	}{
		{[]string{"croc", "chat"}, true}, {[]string{"croc", "chat", "--join"}, true},
		{[]string{"croc", "--relay", "localhost:9009", "chat", "--join"}, true},
		{[]string{"croc", "--remember", "chat"}, false}, {[]string{"croc", "--curve", "p384", "chat"}, false},
		{[]string{"croc", "chat", "secret-in-argv"}, false},
	} {
		app := newApp()
		found := false
		for _, command := range app.Commands {
			if command.Name == "chat" {
				found = true
				command.Action = func(c *cli.Context) error { return validateChatOptions(c) }
			}
		}
		if !found {
			t.Fatal("missing chat command")
		}
		err := app.Run(test.args)
		if (err == nil) != test.valid {
			t.Fatalf("%v: %v", test.args, err)
		}
	}
}
func TestChatRejectsNonTerminal(t *testing.T) {
	// Tests run with standard streams redirected by go test.
	t.Setenv("CROC_SECRET", "")
	err := newApp().Run([]string{"croc", "chat"})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("expected terminal error, got %v", err)
	}
}
func TestChatEditorChunkingAndEscape(t *testing.T) {
	var editor chatEditor
	var lines []string
	submit := func(s string) bool { lines = append(lines, s); return true }
	for _, b := range []byte("Chào 👋\x1b[D!\r") {
		editor.feed([]byte{b}, submit)
	}
	if len(lines) != 1 || lines[0] != "Chào 👋!" {
		t.Fatalf("incorrect input: %q", lines)
	}
	editor.feed([]byte("draft\r"), func(string) bool { return false })
	if string(editor.draft) != "draft" {
		t.Fatal("failed submission lost draft")
	}
	editor.feed([]byte{127}, submit)
	if string(editor.draft) != "draf" {
		t.Fatal("backspace failed")
	}
}
func TestChatSanitizerAndDraftWidth(t *testing.T) {
	text := safeChatText("\x1b]52;c;secret\a\r\u202eFAKE\nPeer\ttext")
	if strings.ContainsAny(text, "\x1b\a\u202e") {
		t.Fatal("unsafe control remained")
	}
	if !strings.Contains(text, "\r\n  Peer    text") {
		t.Fatalf("bad multiline indentation %q", text)
	}
	tail := chatDraftTail([]rune("rất dài 你好 👋"), 12)
	if !utf8.ValidString(tail) {
		t.Fatal("draft truncated UTF-8")
	}
}
func TestChatInvitationHiddenAndCancellation(t *testing.T) {
	input := make(chan chatInputResult, 1)
	input <- chatInputResult{data: []byte("acid-acorn-acre-acts-ahead-alien\r")}
	var out strings.Builder
	code, err := readChatInvitation(context.Background(), input, &out)
	if err != nil || code != "acid-acorn-acre-acts-ahead-alien" {
		t.Fatalf("invitation: %v", err)
	}
	if strings.Contains(out.String(), "acid") {
		t.Fatal("echoed invitation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readChatInvitation(ctx, make(chan chatInputResult), io.Discard); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestChatEditorDoesNotSilentlyTruncateOversizedMessage(t *testing.T) {
	var e chatEditor
	text := strings.Repeat("x", (16<<10)+1)
	var submitted string
	e.feed([]byte(text+"\r"), func(s string) bool { submitted = s; return false })
	if submitted != text || string(e.draft) != text {
		t.Fatal("oversized draft was silently truncated")
	}
	e.feed([]byte{21}, func(string) bool { return true })
	if len(e.draft) != 0 || e.overflow {
		t.Fatal("Ctrl+U did not clear draft")
	}
}

func TestChatEditorStopsAtQuitInPaste(t *testing.T) {
	var editor chatEditor
	var submitted []string
	stopped := editor.feed([]byte("/quit\rshould-not-be-sent\r"), func(line string) bool {
		submitted = append(submitted, line)
		if line == "/quit" {
			editor.stopped = true
		}
		return true
	})
	if !stopped || len(submitted) != 1 || submitted[0] != "/quit" {
		t.Fatalf("processed input after quit: %q", submitted)
	}
	if len(editor.partial) != 0 {
		t.Fatal("kept pasted input after quit")
	}
}
func TestChatEditorIgnoresSS3AndLineSeparators(t *testing.T) {
	var editor chatEditor
	editor.feed([]byte("hello\x1bOA\u2028\u2029world"), func(string) bool { return true })
	if string(editor.draft) != "helloworld" {
		t.Fatalf("unexpected draft: %q", editor.draft)
	}
}

func TestChatWindowsConsoleUnicodeAndRepeat(t *testing.T) {
	record := func(ch uint16, repeat uint16) [20]byte {
		var b [20]byte
		b[0] = 1
		b[4] = 1
		binary.LittleEndian.PutUint16(b[8:10], repeat)
		binary.LittleEndian.PutUint16(b[14:16], ch)
		return b
	}
	var keys chatConsoleKeys
	if out := keys.event(record(0xd83d, 1)); len(out) != 0 {
		t.Fatal("emitted incomplete UTF-16 pair")
	}
	if out := keys.event(record(0xdc4b, 1)); string(out) != "👋" {
		t.Fatalf("surrogate pair: %q", out)
	}
	if out := keys.event(record('é', 3)); string(out) != "é" {
		t.Fatal("wrong first key")
	}
	if string(keys.next()) != "é" || string(keys.next()) != "é" || keys.next() != nil {
		t.Fatal("wrong repeat expansion")
	}
	keyUp := record('x', 1)
	keyUp[4] = 0
	if out := keys.event(keyUp); len(out) != 0 {
		t.Fatal("treated key-up as input")
	}
	if out := keys.event(record(3, 1)); string(out) != "\x03" {
		t.Fatal("lost Ctrl+C")
	}
	if out := keys.event(record('x', 65535)); string(out) != "x" || keys.remaining != 65534 {
		t.Fatal("invalid bounded repeat state")
	}
}
