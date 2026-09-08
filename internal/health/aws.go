package health

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// AWSChecker verifies that an AWS CLI profile still has working credentials.
//
// This is the canary the whole service is built around: SSO sessions expire
// on their own schedule, and a cron job that runs against an expired one
// burns tokens to produce a confidently wrong answer.
type AWSChecker struct {
	// Binary is the aws CLI path. Empty means it was not found.
	Binary string
	// ConfigPath overrides ~/.aws/config, for tests.
	ConfigPath string

	Timeout  time.Duration
	CacheFor time.Duration
}

// Kind implements Checker.
func (c *AWSChecker) Kind() string { return store.KindAWSProfile }

// TTL implements Checker. SSO sessions expire independently of anything the
// scheduler does, so results are kept only briefly.
func (c *AWSChecker) TTL() time.Duration {
	if c.CacheFor > 0 {
		return c.CacheFor
	}
	return 5 * time.Minute
}

func (c *AWSChecker) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 30 * time.Second
}

func (c *AWSChecker) configPath() string {
	if c.ConfigPath != "" {
		return c.ConfigPath
	}
	if v := os.Getenv("AWS_CONFIG_FILE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", "config")
}

// Targets lists configured profile names, read from the config file rather
// than by invoking the CLI so the health page still works when the binary is
// missing.
func (c *AWSChecker) Targets(ctx context.Context) ([]string, error) {
	profiles, err := c.parseProfiles()
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// profile describes what the config file says about one profile, which
// determines the remediation offered.
type profile struct {
	Name          string
	SSOStartURL   string
	SSOSession    string
	RoleARN       string
	SourceProfile string
}

// IsSSO reports whether the profile authenticates through IAM Identity
// Center, directly or by chaining from a profile that does.
func (p profile) IsSSO() bool { return p.SSOStartURL != "" || p.SSOSession != "" }

// parseProfiles reads ~/.aws/config. Sections are [default], [profile name]
// and [sso-session name]; only the first two are profiles.
func (c *AWSChecker) parseProfiles() (map[string]profile, error) {
	path := c.configPath()
	if path == "" {
		return nil, errors.New("could not determine the AWS config location")
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No config is a legitimate state, not a failure.
			return map[string]profile{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	profiles := make(map[string]profile)
	var current string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(line[1 : len(line)-1])
			switch {
			case section == "default":
				current = "default"
			case strings.HasPrefix(section, "profile "):
				current = strings.TrimSpace(strings.TrimPrefix(section, "profile "))
			default:
				// [sso-session foo] and anything else: not a profile.
				current = ""
			}
			if current != "" {
				profiles[current] = profile{Name: current}
			}
			continue
		}

		if current == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		p := profiles[current]
		switch key {
		case "sso_start_url":
			p.SSOStartURL = value
		case "sso_session":
			p.SSOSession = value
		case "role_arn":
			p.RoleARN = value
		case "source_profile":
			p.SourceProfile = value
		}
		profiles[current] = p
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return profiles, nil
}

// callerIdentity is the useful part of `aws sts get-caller-identity`.
type callerIdentity struct {
	UserID  string `json:"UserId"`
	Account string `json:"Account"`
	ARN     string `json:"Arn"`
}

// awsErrorCode captures the code from botocore's standard phrasing,
// "An error occurred (ExpiredToken): ...". The prefix is required: AWS also
// uses parentheses for other things, notably the profile name in
// "The config profile (name) could not be found", and matching those as
// error codes silently misclassifies them.
var awsErrorCode = regexp.MustCompile(`An error occurred \(([A-Za-z][A-Za-z0-9]*)\)`)

// Check probes one profile with sts get-caller-identity, the only
// authoritative test of whether credentials actually work.
func (c *AWSChecker) Check(ctx context.Context, target string) Result {
	start := time.Now()
	res := Result{Kind: c.Kind(), Target: target, CheckedAt: start}

	if c.Binary == "" {
		res.State = store.CheckUnavailable
		res.Detail = "the aws CLI was not found on this system"
		res.Latency = time.Since(start)
		return res
	}

	profiles, err := c.parseProfiles()
	if err != nil {
		res.State = store.CheckMisconfigured
		res.Detail = err.Error()
		res.Latency = time.Since(start)
		return res
	}
	prof, known := profiles[target]
	if !known {
		res.State = store.CheckMisconfigured
		res.Detail = fmt.Sprintf("profile %q is not present in the AWS config", target)
		res.Latency = time.Since(start)
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Binary,
		"sts", "get-caller-identity", "--profile", target, "--output", "json")
	// AWS_PAGER is cleared so the CLI cannot block waiting on a pager.
	cmd.Env = append(os.Environ(), "AWS_PAGER=")

	stdout, runErr := cmd.Output()
	res.Latency = time.Since(start)

	var stderr string
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		stderr = string(ee.Stderr)
	}

	if runErr == nil {
		var id callerIdentity
		if err := json.Unmarshal(stdout, &id); err != nil || id.Account == "" {
			res.State = store.CheckUnknown
			res.Detail = "sts get-caller-identity succeeded but its output could not be parsed"
			return res
		}
		res.State = store.CheckOK
		res.Detail = fmt.Sprintf("account %s as %s", id.Account, shortARN(id.ARN))
		return res
	}

	if ctx.Err() != nil {
		res.State = store.CheckUnavailable
		res.Detail = fmt.Sprintf("sts get-caller-identity timed out after %s", c.timeout())
		return res
	}

	state, detail := classifyAWSError(stderr, runErr)
	res.State = state
	res.Detail = detail
	if state == store.CheckNeedsLogin {
		res.Remediation = ssoRemediation(prof)
	}
	return res
}

// classifyAWSError maps a failed sts call onto a state.
//
// Classification prefers the parenthesised AWS error code over the message
// text. The one case with no code is the SSO-expired message, matched on a
// distinctive fragment. Deciding "needs login" versus "unavailable"
// correctly is what makes auto-pausing a schedule safe.
func classifyAWSError(stderr string, runErr error) (State, string) {
	lower := strings.ToLower(stderr)
	detail := firstErrorLine(stderr)
	if detail == "" && runErr != nil {
		detail = runErr.Error()
	}

	code := ""
	if m := awsErrorCode.FindStringSubmatch(stderr); len(m) == 2 {
		code = m[1]
	}

	switch code {
	case "ExpiredToken", "ExpiredTokenException", "NoCredentials",
		"InvalidClientTokenId", "UnrecognizedClientException", "TokenRefreshRequired":
		return store.CheckNeedsLogin, detail
	case "AccessDenied", "AccessDeniedException":
		// The credentials work; the permissions do not. A login will not fix
		// this, so it is a configuration problem.
		return store.CheckMisconfigured, detail
	case "ProfileNotFound", "InvalidConfigError", "ConfigParseError":
		return store.CheckMisconfigured, detail
	case "EndpointConnectionError", "ConnectTimeoutError", "ReadTimeoutError":
		return store.CheckUnavailable, detail
	}

	// Messages that carry no botocore error code, matched on their
	// distinctive wording. The AWS CLI does not localise these.
	if strings.Contains(lower, "config profile") && strings.Contains(lower, "could not be found") {
		return store.CheckMisconfigured, detail
	}
	if strings.Contains(lower, "profile") && strings.Contains(lower, "does not exist") {
		return store.CheckMisconfigured, detail
	}

	// The SSO-expired path also carries no code.
	if strings.Contains(lower, "sso session") &&
		(strings.Contains(lower, "expired") || strings.Contains(lower, "invalid")) {
		return store.CheckNeedsLogin, detail
	}
	if strings.Contains(lower, "sso") && strings.Contains(lower, "login") {
		return store.CheckNeedsLogin, detail
	}
	// Network-shaped failures are transient and must not gate a schedule.
	if strings.Contains(lower, "could not connect") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "temporary failure in name resolution") ||
		strings.Contains(lower, "network is unreachable") {
		return store.CheckUnavailable, detail
	}

	// Unknown does not gate: refusing to run on an unrecognised error would
	// make an unfamiliar message look like a dead credential.
	return store.CheckUnknown, detail
}

// firstErrorLine extracts the most useful line of AWS CLI stderr.
func firstErrorLine(stderr string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Strip the CLI's own prefix: "aws: [ERROR]: <message>".
		if _, after, found := strings.Cut(line, "[ERROR]:"); found {
			return strings.TrimSpace(after)
		}
		return line
	}
	return ""
}

// ssoRemediation describes how to restore a profile's credentials.
func ssoRemediation(p profile) *Remediation {
	// A role-chaining profile is fixed by logging in to the profile it
	// sources, not to itself.
	loginTarget := p.Name
	if !p.IsSSO() && p.SourceProfile != "" {
		loginTarget = p.SourceProfile
	}

	if p.IsSSO() || p.SourceProfile != "" {
		return &Remediation{
			Kind:      "aws_sso_login",
			Command:   fmt.Sprintf("aws sso login --profile %s", loginTarget),
			Automatic: true,
			Hint: "The scheduler can start this login and show you the verification " +
				"code to enter in your browser.",
		}
	}

	return &Remediation{
		Kind:      "manual_command",
		Command:   fmt.Sprintf("aws configure --profile %s", p.Name),
		Automatic: false,
		Hint:      "This profile uses static credentials, so it must be reconfigured by hand.",
	}
}

// LoginTarget reports which profile must be logged in to restore the given
// one, following a single source_profile hop.
func (c *AWSChecker) LoginTarget(target string) string {
	profiles, err := c.parseProfiles()
	if err != nil {
		return target
	}
	p, ok := profiles[target]
	if !ok {
		return target
	}
	if !p.IsSSO() && p.SourceProfile != "" {
		return p.SourceProfile
	}
	return target
}

// shortARN trims an ARN to its trailing identity portion for display.
func shortARN(arn string) string {
	if idx := strings.LastIndex(arn, "/"); idx >= 0 && idx < len(arn)-1 {
		return arn[idx+1:]
	}
	return arn
}
