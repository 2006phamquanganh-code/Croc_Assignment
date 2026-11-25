package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/schollz/croc/v11/internal/cli"
	"github.com/schollz/croc/v11/src/chat"
	"github.com/schollz/croc/v11/src/comm"
	log "github.com/schollz/croc/v11/src/logger"
	"golang.org/x/term"
)

func validateChatOptions(c *cli.Context) error {
	if c.Args().Present() {
		return errors.New("use croc chat --join and paste the invitation at the prompt")
	}
	allowed := map[string]bool{"join": true, "relay": true, "relay6": true, "pass": true, "socks5": true, "connect": true, "debug": true, "no-update-check": true, "disable-clipboard": true}
	for _, name := range c.FlagNames() {
		if c.IsSet(name) && !allowed[name] {
			return fmt.Errorf("--%s is not supported by croc chat", name)
		}
	}
	if c.IsSet("relay") && c.IsSet("relay6") {
		return errors.New("croc chat accepts only one explicit relay")
	}
	return nil
}

func chatSession(c *cli.Context) error {
	if err := validateChatOptions(c); err != nil {
		return err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("croc chat requires an interactive input and output terminal")
	}
	if c.Bool("debug") {
		log.SetLevel("debug")
	} else {
		log.SetLevel("info")
	}
	comm.Socks5Proxy = c.String("socks5")
	comm.HttpProxy = c.String("connect")
	cfg := chat.Config{RelayPassword: determinePass(c)}
	if c.IsSet("relay") {
		cfg.RelayAddress = strings.TrimSpace(c.String("relay"))
	}
	if c.IsSet("relay6") {
		cfg.RelayAddress = strings.TrimSpace(c.String("relay6"))
	}
	if (c.IsSet("relay") || c.IsSet("relay6")) && cfg.RelayAddress == "" {
		return errors.New("chat relay address must not be empty")
	}
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), state)
	restoreConsole, err := configureChatConsole(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}
	defer restoreConsole()
	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()
	input := make(chan chatInputResult, 16)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		defer close(input)
		reader := chatInput{file: os.Stdin}
		for {
			data, err := reader.read(ctx)
			select {
			case input <- chatInputResult{data, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); <-inputDone }()
	secret := strings.TrimSpace(os.Getenv("CROC_SECRET"))
	if c.Bool("join") && secret == "" {
		fmt.Fprint(os.Stdout, "Enter chat invitation (hidden): ")
		secret, err = readChatInvitation(ctx, input, os.Stdout)
		if err != nil {
			return err
		}
	}
	type connected struct {
		s   *chat.Session
		err error
	}
	admission := make(chan connected, 1)
	go func() {
		var s *chat.Session
		var e error
		if secret != "" {
			s, e = chat.Join(ctx, cfg, secret)
		} else {
			s, e = chat.StartHost(ctx, cfg)
		}
		admission <- connected{s, e}
	}()
	fmt.Fprint(os.Stdout, "Connecting to chat relay…\r\n")
	// Raw mode delivers Ctrl+C as input; keep cancellation responsive during admission.
	var s *chat.Session
	for s == nil {
		select {
		case <-ctx.Done():
			result := <-admission
			if result.s != nil {
				result.s.Close()
				result.s.Wait()
			}
			return ctx.Err()
		case result := <-admission:
			if result.err != nil {
				return result.err
			}
			s = result.s
		case in, ok := <-input:
			if !ok || in.err != nil || strings.ContainsAny(string(in.data), "\x03\x04") {
				cancel()
			}
		}
	}
	defer func() { s.Close(); s.Wait() }()
	if secret == "" {
		fmt.Fprintf(os.Stdout, "Chat invitation: %s\r\nRelay: %s\r\nShare the invitation privately. Join with: croc --relay %s chat --join\r\n", s.Code(), safeChatText(s.RelayAddress()), safeChatText(s.RelayAddress()))
		fmt.Fprint(os.Stdout, "Use the same --pass setting on the joining client if the relay requires one.\r\n")
	}
	fmt.Fprint(os.Stdout, "Waiting for authentication. Ctrl+C exits.\r\n")
	return runChatUI(ctx, s, input, os.Stdout)
}
