// Package ipc owns the Unix socket the daemon serves on and the client side
// that reaches it.
//
// The socket is the whole access-control story: there is no token, so
// whoever can write to the socket file can start a run. That trades a
// network surface for filesystem permissions, which is the right trade for
// a service whose API executes code as the user who owns it.
package ipc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrAlreadyServing means another daemon holds the socket. It is a distinct
// error because "someone else is already running" needs a different remedy
// from every other bind failure.
var ErrAlreadyServing = errors.New("already serving")

// probeTimeout bounds the "is anyone there" connect. It only ever talks to a
// local socket, so a listener that has not answered in this long is not
// going to.
const probeTimeout = 2 * time.Second

// listener pairs the socket with the lock that proves ownership of it.
type listener struct {
	*net.UnixListener
	lock *os.File
}

func (l *listener) Close() error {
	// Order matters. UnixListener.Close unlinks the socket, so the lock has
	// to outlive it: releasing the lock first would let a racing start bind
	// a new socket at this path before the unlink of the old one landed,
	// and the unlink would then remove the new daemon's socket.
	err := l.UnixListener.Close()
	l.lock.Close() // closing the descriptor releases the flock
	return err
}

// Listen binds path, taking over a socket left behind by a dead daemon but
// never displacing a live one.
//
// bind(2) fails with EADDRINUSE for any pre-existing path, whether or not a
// process is listening, so the file alone cannot tell a stale socket from a
// live one. Probing with a connect can, but probe-then-unlink races: two
// daemons starting together both see a refused connection, both unlink, and
// the second removes the socket the first has just bound. They then both
// hold listeners while clients reach only one, and the loser's Close
// unlinks the winner's socket.
//
// So ownership is decided by an flock on a sibling file rather than by the
// socket's presence. The kernel releases an flock when the holding process
// dies, SIGKILL included, which is exactly the case a probe cannot resolve.
// Holding the lock, any socket at the path is provably stale.
func Listen(path string, mode fs.FileMode) (net.Listener, error) {
	// 0750 because the directory is half the access control: it gates who
	// can reach the socket at all. Only created when missing, so an
	// operator's chosen mode is never silently widened.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w on %s", ErrAlreadyServing, path)
		}
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Already the default for a listener net created itself, but stated so
	// that adopting socket activation later, where it defaults to false, is
	// a visible change rather than a silently leaked socket file.
	ln.SetUnlinkOnClose(true)

	// bind(2) creates the socket with 0777 minus the process umask, so the
	// requested mode has to be applied afterwards. The window between the
	// two is fail-closed under any ordinary umask: connecting to a socket
	// needs the write bit, which 0777&^0022 does not grant to group or
	// other, and this chmod only ever widens access from there. The unit
	// sets UMask=0027 so the fully-open case cannot arise.
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		lock.Close()
		return nil, fmt.Errorf("set permissions on %s: %w", path, err)
	}
	return &listener{UnixListener: ln, lock: lock}, nil
}

// Serving reports whether a daemon is accepting connections on path. Every
// client command runs this before anything else, so that a stopped service
// produces one clear message instead of a transport error per request.
func Serving(path string) bool {
	conn, err := net.DialTimeout("unix", path, probeTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Dial connects to the daemon.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// Client returns an HTTP client that reaches the daemon over path.
//
// It deliberately carries no Timeout: `exec logs --follow` holds an SSE
// response open for as long as a run lasts, and a client timeout would cut
// it off mid-transcript. Callers bound their own requests with a context.
func Client(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return Dial(ctx, path)
			},
			// One socket, one peer: the pool only ever holds connections to
			// the same daemon.
			MaxIdleConns:    4,
			IdleConnTimeout: 30 * time.Second,
		},
	}
}

// URL builds a request URL for the daemon. The host is ignored by the unix
// dialer, but http.Client still requires a well-formed absolute URL.
func URL(path string) string { return "http://unix" + path }

// Explain turns a connection failure into something a user can act on.
//
// The cases genuinely differ. ENOENT means nothing has ever bound the path,
// so the service is not running or the path is wrong. ECONNREFUSED means
// the socket file exists but no process is behind it, so the daemon died
// without cleaning up. EACCES means it is running and this user is not
// allowed to reach it, which is a permissions problem and not a start
// problem, and telling someone to restart the service would be wrong.
func Explain(socket string, err error) error {
	switch {
	case errors.Is(err, syscall.EACCES), errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EPERM):
		return fmt.Errorf("permission denied connecting to %s\n"+
			"  the service is running, but this user cannot use its socket\n"+
			"  check the owner and group:  ls -l %s", socket, socket)

	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("%s exists but nothing is listening\n"+
			"  the daemon stopped without cleaning up; a restart clears it\n"+
			"  check it:  systemctl status claude-scheduler", socket)

	case errors.Is(err, syscall.ENOENT), errors.Is(err, os.ErrNotExist):
		return NotServingError(socket)

	case errors.Is(err, syscall.EINVAL):
		return fmt.Errorf("the socket path %s is unusable, most likely longer "+
			"than the %d byte limit: %w", socket, MaxPathLen, err)
	}
	return fmt.Errorf("connect to %s: %w", socket, err)
}

// NotServingError is the message every client command prints when the
// daemon is not up. It names the socket it tried, because a wrong --socket
// or a stale CLAUDE_SCHEDULER_SOCKET looks identical to a stopped service.
func NotServingError(socket string) error {
	return fmt.Errorf("no claude-scheduler is listening on %s\n"+
		"  start it with:  sudo systemctl start claude-scheduler\n"+
		"  or point at another socket with --socket or $CLAUDE_SCHEDULER_SOCKET", socket)
}

// Unreachable reports whether err means the daemon could not be reached, as
// opposed to having answered with a failure. The CLI maps this onto its own
// exit code so a script can tell "not running" from "the command failed".
func Unreachable(err error) bool {
	return errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EINVAL)
}

// MaxPathLen is the usable length of a Unix socket path. sun_path is a fixed
// 108-byte field including its terminator.
const MaxPathLen = 107
