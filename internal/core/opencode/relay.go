// relay.go — the namespace-side bridge between the runtime's HTTP client
// and the sidecar's unix socket.
//
// # WHY A RELAY IS NEEDED AT ALL
//
// The worker's network namespace is unshared, which is what makes "no
// unrestricted egress" and "no access to the live OpenCode service" true.
// The runtime still speaks HTTP to a base URL, and in an unshared namespace
// the only address that exists is its OWN loopback. A unix socket is not
// network-namespace-aware, so it is the one channel that crosses the
// boundary — but an HTTP client cannot be pointed at it directly.
//
// So the runtime is pointed at 127.0.0.1 inside the namespace, and this
// relay — which holds NO credential and makes NO decision — pipes each
// connection to the bound sidecar socket.
//
// # WHY IT IS SAFE
//
// It listens on loopback only, inside a namespace that has no route to
// anything else. It does not authenticate, because the only client that can
// reach it is inside the same namespace, and the sidecar behind it enforces
// the model allow-list and the bounds. It cannot be used to reach anything
// but the socket it was given: the destination is fixed at start-up.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// RelayConfig configures the namespace-side bridge.
type RelayConfig struct {
	// Listen is the loopback address inside the namespace.
	Listen string
	// UpstreamSocket is the bound sidecar socket path.
	UpstreamSocket string
}

// RunRelay serves until ctx is cancelled.
//
// It is executed INSIDE the namespace: the adapter spawns the MPM binary
// with RelayEnvVar pointing at the sidecar socket.
func RunRelay(ctx context.Context, cfg RelayConfig) error {
	if cfg.Listen == "" {
		cfg.Listen = RelayAddr
	}
	if cfg.UpstreamSocket == "" {
		return errors.New("opencode: the relay has no upstream socket")
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("opencode: the relay could not listen on %s: %w", cfg.Listen, err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				// Drain in-flight connections so a cancelled relay does
				// not cut a response mid-stream.
				wg.Wait()
				return nil
			}
			return fmt.Errorf("opencode: the relay could not accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pipeToSocket(ctx, cfg.UpstreamSocket, conn)
		}()
	}
}

// pipeToSocket copies one connection to the sidecar socket and back.
func pipeToSocket(ctx context.Context, socketPath string, client net.Conn) error {
	defer client.Close()

	dialer := net.Dialer{Timeout: 10 * time.Second}
	upstream, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("opencode: the relay could not reach the sidecar: %w", err)
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		if c, ok := upstream.(*net.UnixConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if c, ok := client.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	return nil
}

// RelayModeFromEnv reports whether this process was started as the relay,
// and where.
//
// The adapter sets MPM_OPENCODE_RELAY_SOCKET on the sandboxed process. The
// MPM entry point checks this BEFORE any configuration, database or CLI
// work, so the relay never opens the substrate, never resolves a
// workspace and never parses an operator argument.
func RelayModeFromEnv() (socketPath string, ok bool) {
	v := os.Getenv(RelayEnvVar)
	if v == "" {
		return "", false
	}
	return v, true
}
