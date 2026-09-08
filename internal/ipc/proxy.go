package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

// halfCloser is implemented by both TCP and Unix connections. Closing one
// direction at a time is what lets a request finish while the response is
// still streaming.
type halfCloser interface {
	CloseWrite() error
}

// Proxy pipes a TCP listener into the daemon's socket, byte for byte.
//
// It copies bytes rather than parsing HTTP on purpose. The API streams
// Server-Sent Events for a whole run, reuses keep-alive connections, and
// sets X-Accel-Buffering to defeat exactly the kind of buffering an
// intermediate HTTP proxy introduces. A byte pipe has none of those
// concerns and nothing to keep in sync with the API.
func Proxy(ctx context.Context, ln net.Listener, socket string, log *slog.Logger) error {
	var wg sync.WaitGroup

	// Unblock Accept on shutdown; the accept loop reads ctx to tell a
	// deliberate close from a real failure.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				// Let connections in flight finish rather than cutting a
				// transcript off mid-stream.
				wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pipe(conn, socket); err != nil {
				log.Debug("proxied connection ended", "error", err)
			}
		}()
	}
}

// pipe joins one accepted connection to a fresh connection to the socket.
func pipe(client net.Conn, socket string) error {
	defer client.Close()

	server, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("dial %s: %w", socket, err)
	}
	defer server.Close()

	// Each direction closes its write side as it finishes, so the peer sees a
	// clean EOF and can respond, instead of waiting on a connection that is
	// only half done. Tearing the whole connection down on the first
	// completed direction would truncate the response to a request that had
	// just finished uploading.
	errs := make(chan error, 2)
	go func() { errs <- copyThenCloseWrite(server, client) }()
	go func() { errs <- copyThenCloseWrite(client, server) }()

	var firstErr error
	for range 2 {
		if err := <-errs; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func copyThenCloseWrite(dst, src net.Conn) error {
	_, err := io.Copy(dst, src)
	if hc, ok := dst.(halfCloser); ok {
		hc.CloseWrite()
	}
	// A peer hanging up mid-copy is normal for a browser that navigated away
	// or a client that stopped following a stream.
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}
