package chat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/schollz/croc/v11/src/comm"
	"github.com/schollz/croc/v11/src/message"
	"github.com/schollz/croc/v11/src/tcp"
	"github.com/schollz/peerdiscovery"
)

const (
	chatLocalDiscoveryPrefix = "croc-chat:"
	defaultMulticastAddress  = "239.255.255.250"
	localDiscoveryTimeout    = 800 * time.Millisecond
	onlyLocalDiscoveryWindow = 2 * time.Second
)

var chatPeerDiscover = peerdiscovery.Discover

func shouldUseLocal(cfg Config) bool {
	if cfg.DisableLocal {
		return false
	}
	if cfg.OnlyLocal {
		return true
	}
	if strings.TrimSpace(cfg.RelayAddress) != "" || comm.Socks5Proxy != "" || comm.HttpProxy != "" {
		return false
	}
	return true
}

func multicastAddressOrDefault(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return defaultMulticastAddress
	}
	return addr
}

func parseChatDiscoveryAddresses(discoveries []peerdiscovery.Discovered) []string {
	seen := make(map[string]struct{}, len(discoveries))
	var addresses []string
	prefix := []byte(chatLocalDiscoveryPrefix)
	for _, d := range discoveries {
		if !bytes.HasPrefix(d.Payload, prefix) {
			continue
		}
		portStr := strings.TrimSpace(string(bytes.TrimPrefix(d.Payload, prefix)))
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		if strings.TrimSpace(d.Address) == "" {
			continue
		}
		addr := net.JoinHostPort(d.Address, strconv.Itoa(port))
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		addresses = append(addresses, addr)
	}
	return addresses
}

type localHostRelay struct {
	conn          *comm.Comm
	addr          string
	stopBroadcast func()
	stopRelay     func()
}

func startLocalHostRelay(ctx context.Context, roomName, password, multicastAddr string) (*localHostRelay, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	relayCtx, cancelRelay := context.WithCancel(context.Background())
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		_ = tcp.RunWithListener(listener, password, tcp.WithCtx(relayCtx), tcp.WithLogLevel("error"), tcp.WithBanner(port))
	}()

	stopBroadcastChan := make(chan struct{})
	var broadcastOnce sync.Once
	stopBroadcast := func() {
		broadcastOnce.Do(func() {
			close(stopBroadcastChan)
		})
	}

	var relayOnce sync.Once
	stopRelay := func() {
		stopBroadcast()
		relayOnce.Do(func() {
			cancelRelay()
			<-relayDone
		})
	}

	localAddr := net.JoinHostPort("127.0.0.1", port)
	dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDial()
	conn, _, _, _, err := tcp.ConnectToTCPServerControlContext(dialCtx, localAddr, password, roomName, 5*time.Second)
	if err != nil {
		stopRelay()
		return nil, err
	}

	payload := []byte(chatLocalDiscoveryPrefix + port)
	mcast := multicastAddressOrDefault(multicastAddr)
	go broadcastChatLocal(payload, mcast, false, stopBroadcastChan)
	go broadcastChatLocal(payload, mcast, true, stopBroadcastChan)

	return &localHostRelay{
		conn:          conn,
		addr:          "LAN :" + port,
		stopBroadcast: stopBroadcast,
		stopRelay:     stopRelay,
	}, nil
}

func broadcastChatLocal(payload []byte, multicastAddr string, ipv6 bool, stopChan chan struct{}) {
	settings := peerdiscovery.Settings{
		Limit:     -1,
		Payload:   payload,
		Delay:     50 * time.Millisecond,
		TimeLimit: -1 * time.Second,
		StopChan:  stopChan,
	}
	if ipv6 {
		settings.IPVersion = peerdiscovery.IPv6
	} else {
		settings.MulticastAddress = multicastAddr
	}
	_, _ = chatPeerDiscover(settings)
}

func discoverLocalChatAddresses(ctx context.Context, multicastAddr string, window time.Duration) []string {
	mcast := multicastAddressOrDefault(multicastAddr)
	stopChan := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { close(stopChan) })
	}
	defer stop()

	stopAfter := context.AfterFunc(ctx, stop)
	defer stopAfter()

	results := make(chan []peerdiscovery.Discovered, 2)
	run := func(ipv6 bool) {
		settings := peerdiscovery.Settings{
			Limit:            -1,
			Payload:          []byte("croc-chat-probe"),
			Delay:            20 * time.Millisecond,
			TimeLimit:        window,
			StopChan:         stopChan,
			AllowSelf:        true,
			DisableBroadcast: true,
			Notify: func(d peerdiscovery.Discovered) {
				if len(parseChatDiscoveryAddresses([]peerdiscovery.Discovered{d})) > 0 {
					stop()
				}
			},
		}
		if ipv6 {
			settings.IPVersion = peerdiscovery.IPv6
		} else {
			settings.MulticastAddress = mcast
		}
		found, err := chatPeerDiscover(settings)
		if err != nil {
			results <- nil
			return
		}
		results <- found
	}

	go run(false)
	go run(true)

	var all []peerdiscovery.Discovered
	timer := time.NewTimer(window + 150*time.Millisecond)
	defer timer.Stop()
	for range 2 {
		select {
		case found := <-results:
			all = append(all, found...)
		case <-ctx.Done():
			stop()
			return nil
		case <-timer.C:
			stop()
			return parseChatDiscoveryAddresses(all)
		}
	}
	return parseChatDiscoveryAddresses(all)
}

func tryJoinLocalChat(ctx context.Context, roomName, password, multicastAddr string, window time.Duration) (*comm.Comm, string, error) {
	candidates := discoverLocalChatAddresses(ctx, multicastAddr, window)
	for _, addr := range candidates {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		dialCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		c, _, _, _, err := tcp.ConnectToTCPServerControlContext(dialCtx, addr, password, roomName, 1*time.Second)
		cancel()
		if err == nil {
			return c, addr, nil
		}
	}
	return nil, "", errors.New("could not find a local chat host on LAN")
}

type hostRaceResult struct {
	conn    *comm.Comm
	first   message.Message
	isLocal bool
	err     error
}

// raceHostConnections waits concurrently on the host's local LAN relay and
// external relay connections, returning the first peer that sends a valid PAKE
// initiation frame and closing the losing connection.
func (s *Session) raceHostConnections(wait time.Duration) (*comm.Comm, *message.Message, bool, error) {
	deadline := time.Now().Add(wait)
	s.mu.Lock()
	localConn := s.localConn
	relayConn := s.conn
	s.mu.Unlock()

	results := make(chan hostRaceResult, 2)
	go func() {
		first, err := receiveHandshake(localConn, deadline, false, false)
		results <- hostRaceResult{conn: localConn, first: first, isLocal: true, err: err}
	}()
	go func() {
		first, err := receiveHandshake(relayConn, deadline, false, false)
		results <- hostRaceResult{conn: relayConn, first: first, isLocal: false, err: err}
	}()

	var firstErr error
	for i := range 2 {
		res := <-results
		if res.err == nil {
			if res.isLocal {
				relayConn.Close()
			} else {
				localConn.Close()
			}
			// Wait for the losing reader goroutine to exit cleanly.
			if i == 0 {
				<-results
			}
			return res.conn, &res.first, res.isLocal, nil
		}
		if firstErr == nil {
			firstErr = res.err
		}
	}
	return nil, nil, false, firstErr
}

// formatTransportStats formats a concise connection transport label.
func formatTransportStats(isLocal bool, addr string) string {
	if isLocal {
		if addr != "" {
			return fmt.Sprintf("Direct LAN (%s)", addr)
		}
		return "Direct LAN"
	}
	if addr != "" {
		return fmt.Sprintf("Relay Server (%s)", addr)
	}
	return "Relay Server"
}
