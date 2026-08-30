// internal/telemetry/serve.go — accept loop + per-connection handler.
//
// Per spec §3: one goroutine per accepted connection; parse, validate,
// persist, encode response, close. No goroutine pool. The collector is
// single-process and write-only; contention is not the bottleneck.

package telemetry

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Server holds collector state visible to connections.
type Server struct {
	StartedAt time.Time
}

// Serve starts the Unix-socket collector and blocks until ctx is cancelled.
// The returned Server carries the start time used for uptime_seconds in pings.
func Serve(ctx context.Context, store *Store, socketPath string) error {
	srv := &Server{StartedAt: time.Now()}
	return serve(ctx, store, socketPath, srv)
}

func serve(ctx context.Context, store *Store, socketPath string, srv *Server) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Remove any stale socket file from a previous run.
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	defer func() {
		// Close the listener (releases the FD) AND unlink the socket
		// file. Go's net.UnixListener.Close releases the FD but does
		// NOT remove the socket inode from the filesystem, leaving a
		// stale entry that confuses the next serve start. Unlinking
		// here ensures the workspace is clean on shutdown.
		listener.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			handleConn(c, store, srv)
		}(conn)
	}
}

func handleConn(c net.Conn, store *Store, srv *Server) {
	defer c.Close()
	for {
		f, err := DecodeFrame(c)
		if err != nil {
			ve, ok := AsValidationError(err)
			if ok {
				_ = EncodeResponse(c, Response{Status: StatusRejected, Reason: ve.Reason})
				return
			}
			// EOF or unrecoverable scan error: close cleanly.
			return
		}
		if f.EventType == "ping" {
			_ = EncodeResponse(c, Response{
				Status:           StatusAccepted,
				CollectorVersion: BuildVersion,
				ProtocolVersion:  SchemaVersion,
				SchemaVersion:    SchemaVersion,
				QueueDepth:       0, // single-goroutine per conn in v1
				UptimeSeconds:    int64(time.Since(srv.StartedAt).Seconds()),
			})
			continue
		}
		res, err := store.InsertFrame(context.Background(), f)
		if err != nil {
			_ = EncodeResponse(c, Response{Status: StatusDropped, Reason: "persistence_failed"})
			continue
		}
		if res.Conflict {
			_ = EncodeResponse(c, Response{Status: StatusRejected, Reason: "invocation_id_payload_conflict"})
			continue
		}
		_ = EncodeResponse(c, Response{Status: StatusAccepted, Inserted: res.Inserted})
	}
}
