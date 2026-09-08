package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/raulsh/claude-scheduler/internal/config"
	"github.com/raulsh/claude-scheduler/internal/ipc"
)

// runProxy serves a loopback TCP port that relays into the daemon's socket,
// for the browser UI and for Vite's dev proxy.
//
// It is a separate command, never started by the unit, because it gives away
// the property the socket buys. The socket at 0660 inside a 0750 directory
// is reachable by one user and one group; a loopback port with no
// authentication is reachable by every local user, and this API can start
// Claude Code runs. Loopback is not a trust boundary on a shared machine, so
// running this is strictly a widening of access and says so out loud.
func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "configuration file path")
	socket := fs.String("socket", "", "socket to forward to")
	listen := fs.String("listen", "127.0.0.1:9977", "loopback address to serve")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(*logLevel)

	addr, err := loopbackAddr(*listen)
	if err != nil {
		return err
	}

	target := *socket
	if target == "" {
		// The proxy reads the same config as the daemon, but only for the
		// socket path: it has no use for the rest.
		if cfg, err := config.Load(*configPath); err == nil {
			target = cfg.Server.Socket
		} else {
			target = config.DefaultSocket
		}
	}

	// Fail now rather than turning every future request into an unexplained
	// empty reply.
	if !ipc.Serving(target) {
		return ipc.NotServingError(target)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	log.Warn("serving the API over TCP with no authentication; "+
		"any local user can reach it and start runs as this user",
		"listen", addr, "socket", target)
	log.Info("proxying", "from", "http://"+addr, "to", target)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return ipc.Proxy(ctx, ln, target, log)
}

// loopbackAddr validates the listen address.
//
// netip rather than a string comparison, because the addresses that need
// accepting and rejecting do not fall into a small set of spellings:
// 127.0.0.2 and ::ffff:127.0.0.1 are loopback, and a name could resolve
// anywhere at all, so names are refused rather than resolved.
func loopbackAddr(spec string) (string, error) {
	host, port, err := net.SplitHostPort(spec)
	if err != nil {
		return "", fmt.Errorf("--listen %q must be host:port, such as 127.0.0.1:9977", spec)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("--listen host %q must be an IP address rather than a name, "+
			"since a name could resolve off this machine", host)
	}
	if !ip.IsLoopback() {
		return "", fmt.Errorf("refusing to listen on %s: this proxy performs no "+
			"authentication, so it may only bind loopback.\n"+
			"  the daemon's access control is its socket's file permissions, and "+
			"forwarding from a routable address discards that entirely", ip)
	}
	return net.JoinHostPort(ip.String(), port), nil
}
