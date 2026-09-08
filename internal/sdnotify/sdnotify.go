// Package sdnotify implements the small part of the systemd notification
// protocol this service needs, so the unit can use Type=notify without
// pulling in a dependency.
package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// Ready tells systemd the service is up and accepting connections.
func Ready() error { return send("READY=1") }

// Stopping tells systemd shutdown has begun, so it does not treat the
// closing socket as a failure.
func Stopping() error { return send("STOPPING=1") }

// Status sets the one-line status shown by `systemctl status`.
func Status(msg string) error { return send("STATUS=" + msg) }

// send writes a notification datagram. It is a no-op when NOTIFY_SOCKET is
// unset, which is the case whenever the binary runs outside systemd.
func send(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	// A leading '@' denotes the abstract socket namespace, which Go spells
	// with a leading NUL.
	if strings.HasPrefix(addr, "@") {
		addr = "\x00" + addr[1:]
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("dial notify socket: %w", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("write %q: %w", state, err)
	}
	return nil
}
