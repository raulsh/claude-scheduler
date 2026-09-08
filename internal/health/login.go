package health

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// Login session states.
const (
	LoginStarting  = "starting"
	LoginAwaiting  = "awaiting_authorization"
	LoginCompleted = "completed"
	LoginFailed    = "failed"
	LoginCancelled = "cancelled"
)

// loginTimeout bounds a login attempt. The AWS device flow expires on its
// own well inside this, so it only guards against a hung process.
const loginTimeout = 10 * time.Minute

// LoginSession tracks one `aws sso login` attempt.
//
// This flow is automatable precisely because it is one-way: the CLI prints a
// verification URL and code, then polls until the user authorises in a
// browser. Nothing has to be typed back into the terminal, unlike the MCP
// login flow.
type LoginSession struct {
	Profile string `json:"profile"`
	// LoginProfile is the profile actually logged in to, which differs from
	// Profile when a role-chaining profile sources another.
	LoginProfile string `json:"login_profile"`

	State           string    `json:"state"`
	VerificationURI string    `json:"verification_uri,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at,omitzero"`
	Error           string    `json:"error,omitempty"`
	Output          string    `json:"output,omitempty"`
}

// Done reports whether the session has reached a terminal state.
func (s LoginSession) Done() bool {
	switch s.State {
	case LoginCompleted, LoginFailed, LoginCancelled:
		return true
	default:
		return false
	}
}

// LoginManager runs and tracks AWS SSO login attempts.
type LoginManager struct {
	checker  *AWSChecker
	registry *Registry
	log      *slog.Logger

	mu       sync.Mutex
	sessions map[string]*LoginSession
	cancels  map[string]context.CancelFunc
}

// NewLoginManager creates a manager.
func NewLoginManager(checker *AWSChecker, registry *Registry, log *slog.Logger) *LoginManager {
	return &LoginManager{
		checker:  checker,
		registry: registry,
		log:      log,
		sessions: make(map[string]*LoginSession),
		cancels:  make(map[string]context.CancelFunc),
	}
}

// verificationURI matches the URL the CLI prints for the device flow.
var verificationURI = regexp.MustCompile(`https://\S*\.amazonaws\.com/\S*|https://\S+/\?user_code=\S+|https://device\.sso\.\S+`)

// userCode matches the AWS device code format, four characters, a hyphen,
// four characters.
var userCode = regexp.MustCompile(`\b([A-Z0-9]{4}-[A-Z0-9]{4})\b`)

// Start begins a login for a profile, returning the session immediately so
// the caller can show progress while it runs.
func (m *LoginManager) Start(profile string) (*LoginSession, error) {
	if m.checker.Binary == "" {
		return nil, errors.New("the aws CLI was not found on this system")
	}

	loginProfile := m.checker.LoginTarget(profile)

	m.mu.Lock()
	if existing, running := m.sessions[profile]; running && !existing.Done() {
		snapshot := *existing
		m.mu.Unlock()
		return &snapshot, nil // Idempotent: reuse the attempt in flight.
	}

	session := &LoginSession{
		Profile:      profile,
		LoginProfile: loginProfile,
		State:        LoginStarting,
		StartedAt:    time.Now(),
	}
	m.sessions[profile] = session

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	m.cancels[profile] = cancel
	m.mu.Unlock()

	go m.run(ctx, cancel, profile, loginProfile)

	snapshot := *session
	return &snapshot, nil
}

// run drives the login process, scraping the verification details out of its
// output as they appear.
func (m *LoginManager) run(ctx context.Context, cancel context.CancelFunc, profile, loginProfile string) {
	defer cancel()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, profile)
		m.mu.Unlock()
	}()

	cmd := exec.CommandContext(ctx, m.checker.Binary,
		"sso", "login", "--profile", loginProfile, "--no-browser")
	cmd.Env = append(os.Environ(), "AWS_PAGER=")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.fail(profile, "could not capture output: "+err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		m.fail(profile, "could not start aws sso login: "+err.Error())
		return
	}

	m.log.Info("aws sso login started", "profile", profile, "login_profile", loginProfile)

	// The CLI prints the URL and code, then blocks polling for
	// authorisation, so output must be read incrementally rather than
	// waiting for the process to exit.
	var collected strings.Builder
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		collected.WriteString(line)
		collected.WriteByte('\n')
		m.observeLine(profile, line, collected.String())
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		m.log.Debug("login output read ended", "profile", profile, "error", err)
	}

	waitErr := cmd.Wait()

	m.mu.Lock()
	session := m.sessions[profile]
	if session == nil {
		m.mu.Unlock()
		return
	}
	session.Output = truncateOutput(collected.String())
	session.FinishedAt = time.Now()

	switch {
	case session.State == LoginCancelled:
		// Left as cancelled.
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		session.State = LoginFailed
		session.Error = "the login attempt timed out before it was authorised"
	case waitErr != nil:
		session.State = LoginFailed
		session.Error = loginFailureMessage(collected.String(), waitErr)
	default:
		session.State = LoginCompleted
	}
	state := session.State
	failure := session.Error
	m.mu.Unlock()

	if state == LoginCompleted {
		// The cached needs_login verdict is now stale.
		m.registry.Invalidate(store.KindAWSProfile, profile)
		if loginProfile != profile {
			m.registry.Invalidate(store.KindAWSProfile, loginProfile)
		}
		m.log.Info("aws sso login completed", "profile", profile)
	} else {
		m.log.Warn("aws sso login did not complete",
			"profile", profile, "state", state, "error", failure)
	}
}

// observeLine extracts the verification URL and user code as they are
// printed, so the UI can show them while the CLI is still polling.
func (m *LoginManager) observeLine(profile, line, all string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session := m.sessions[profile]
	if session == nil {
		return
	}

	if session.VerificationURI == "" {
		if uri := verificationURI.FindString(line); uri != "" {
			session.VerificationURI = strings.TrimRight(uri, ".,)")
		}
	}
	if session.UserCode == "" {
		if m := userCode.FindStringSubmatch(line); len(m) == 2 {
			session.UserCode = m[1]
		}
	}

	// Once there is something actionable to show, the user is the one being
	// waited on.
	if session.State == LoginStarting && (session.VerificationURI != "" || session.UserCode != "") {
		session.State = LoginAwaiting
	}
	session.Output = truncateOutput(all)
}

func (m *LoginManager) fail(profile, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if session := m.sessions[profile]; session != nil {
		session.State = LoginFailed
		session.Error = message
		session.FinishedAt = time.Now()
	}
}

// Get returns a snapshot of a profile's login session.
func (m *LoginManager) Get(profile string) (*LoginSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[profile]
	if !ok {
		return nil, false
	}
	snapshot := *session
	return &snapshot, true
}

// Cancel aborts an in-flight login.
func (m *LoginManager) Cancel(profile string) bool {
	m.mu.Lock()
	cancel, running := m.cancels[profile]
	if session := m.sessions[profile]; session != nil && !session.Done() {
		session.State = LoginCancelled
		session.FinishedAt = time.Now()
	}
	m.mu.Unlock()

	if !running {
		return false
	}
	cancel()
	return true
}

// Active lists login sessions that have not finished.
func (m *LoginManager) Active() []LoginSession {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []LoginSession
	for _, s := range m.sessions {
		if !s.Done() {
			out = append(out, *s)
		}
	}
	return out
}

// loginFailureMessage picks the most useful explanation from the CLI output.
func loginFailureMessage(output string, waitErr error) string {
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "[ERROR]") || strings.Contains(line, "error occurred") {
			if _, after, found := strings.Cut(line, "[ERROR]:"); found {
				return strings.TrimSpace(after)
			}
			return line
		}
	}
	return fmt.Sprintf("aws sso login failed: %v", waitErr)
}

func truncateOutput(s string) string {
	const limit = 4000
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}
