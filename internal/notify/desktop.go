package notify

import (
	"context"
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
)

// Desktop sends notifications to the user's graphical session over D-Bus.
//
// A direct D-Bus call is used rather than shelling out to notify-send: it
// avoids a subprocess per notification and returns the notification id, so a
// repeat about the same task can replace the previous popup instead of
// stacking up.
//
// The service runs as a system unit, so it reaches the session bus through
// DBUS_SESSION_BUS_ADDRESS, which the package's generated systemd drop-in
// sets to the service user's /run/user/<uid>/bus.
type Desktop struct {
	// AppName appears as the notification source.
	AppName string

	on bool

	mu   sync.Mutex
	conn *dbus.Conn
	// lastID tracks the previous notification per task, so a follow-up
	// replaces it rather than adding another popup.
	lastID map[string]uint32
}

// NewDesktop creates a desktop notifier.
func NewDesktop(enabled bool) *Desktop {
	return &Desktop{
		AppName: "Claude Scheduler",
		on:      enabled,
		lastID:  make(map[string]uint32),
	}
}

// Name implements Notifier.
func (d *Desktop) Name() string { return "desktop" }

// Enabled implements Notifier.
func (d *Desktop) Enabled() bool { return d.on }

// Close releases the bus connection.
func (d *Desktop) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.conn == nil {
		return nil
	}
	err := d.conn.Close()
	d.conn = nil
	return err
}

// connect returns a session bus connection, dialling lazily so the service
// starts cleanly even when no graphical session exists yet.
func (d *Desktop) connect() (*dbus.Conn, error) {
	if d.conn != nil && d.conn.Connected() {
		return d.conn, nil
	}

	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, fmt.Errorf("authenticate to the session bus: %w", err)
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("session bus handshake: %w", err)
	}

	d.conn = conn
	return conn, nil
}

// Notify implements Notifier.
func (d *Desktop) Notify(ctx context.Context, ev Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	conn, err := d.connect()
	if err != nil {
		return err
	}

	key := ev.Kind + "\x00" + ev.TaskName
	replaces := d.lastID[key]

	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(dbusUrgency(ev.Urgency))),
		// Group related popups in shells that support it.
		"category": dbus.MakeVariant("claude-scheduler." + ev.Kind),
	}

	// A critical notification stays until dismissed; anything else expires.
	timeout := int32(10000)
	if ev.Urgency == UrgencyCritical {
		timeout = 0
	}

	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	call := obj.CallWithContext(ctx, "org.freedesktop.Notifications.Notify", 0,
		d.AppName,
		replaces,
		"dialog-warning",
		ev.Title,
		ev.Body,
		[]string{},
		hints,
		timeout,
	)
	if call.Err != nil {
		// Drop the connection so the next attempt redials; the session bus
		// goes away when the user logs out.
		if d.conn != nil {
			d.conn.Close()
			d.conn = nil
		}
		return fmt.Errorf("Notifications.Notify: %w", call.Err)
	}

	var id uint32
	if err := call.Store(&id); err == nil {
		d.lastID[key] = id
	}
	return nil
}

// dbusUrgency maps our levels onto the freedesktop notification spec.
func dbusUrgency(u Urgency) int {
	switch u {
	case UrgencyLow:
		return 0
	case UrgencyCritical:
		return 2
	default:
		return 1
	}
}
