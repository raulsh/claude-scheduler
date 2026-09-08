package ipc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startProxy runs a proxy in front of ln and returns the TCP address.
func startProxy(t *testing.T, socket string) string {
	t.Helper()

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Proxy(ctx, tcp, socket, quietLogger())
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return tcp.Addr().String()
}

func TestProxyForwardsRequests(t *testing.T) {
	path := socketPath(t)
	ln, err := Listen(path, 0o660)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello %s", r.URL.Path)
		})}
	go srv.Serve(ln)
	defer srv.Close()

	addr := startProxy(t, path)

	resp, err := http.Get("http://" + addr + "/api/v1/health")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello /api/v1/health" {
		t.Errorf("body = %q, want the daemon's response", body)
	}
}

// TestProxySurvivesClientHalfClose is the regression test for the naive
// relay. An HTTP client stops writing as soon as its request is sent, so a
// relay that tore the connection down on the first finished direction would
// truncate a Server-Sent Events response the moment the request body ended.
// Every other proxy test passes against that broken version.
func TestProxySurvivesClientHalfClose(t *testing.T) {
	path := socketPath(t)
	ln, err := Listen(path, 0o660)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	// A stream that keeps emitting after the request is complete.
	srv := &http.Server{Handler: http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			for i := range 3 {
				fmt.Fprintf(w, "data: frame %d\n\n", i)
				flusher.Flush()
				time.Sleep(20 * time.Millisecond)
			}
		})}
	go srv.Serve(ln)
	defer srv.Close()

	addr := startProxy(t, path)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer conn.Close()

	fmt.Fprint(conn, "GET /stream HTTP/1.1\r\nHost: x\r\n\r\n")
	// Half-close: done sending, still reading. This is what a real client
	// does once its request is complete.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)

	frames := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if len(line) > 5 && line[:5] == "data:" {
			frames++
		}
	}
	if frames != 3 {
		t.Errorf("received %d frames after half-closing, want 3: the relay cut "+
			"the response short when the request direction finished", frames)
	}
}

// TestProxyClosesWhenDaemonIsDown asserts the property that matters when
// the socket has nothing behind it: the connection ends promptly instead of
// hanging. No HTTP error is synthesised, because writing one would make this
// a protocol-aware proxy rather than a byte pipe, so the client sees the
// connection go away. A reset rather than a clean FIN is expected and
// normal: the request bytes are still unread when the relay gives up.
func TestProxyClosesWhenDaemonIsDown(t *testing.T) {
	addr := startProxy(t, socketPath(t))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer conn.Close()

	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	_, err = io.ReadAll(conn)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("the connection hung; it should end as soon as the dial fails")
	}
}
