package health

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/raulsh/claude-scheduler/internal/store"
)

// TestClassifyAWSError uses stderr captured from the real AWS CLI v2 on this
// machine. The needs_login / unavailable split is what decides whether a
// failing dependency may pause a schedule.
func TestClassifyAWSError(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   State
	}{
		{
			name:   "no credentials at all (captured)",
			stderr: "\naws: [ERROR]: An error occurred (NoCredentials): Unable to locate credentials. You can configure credentials by running \"aws login\".",
			want:   store.CheckNeedsLogin,
		},
		{
			name:   "expired SSO session, no parenthesised code (captured)",
			stderr: "\naws: [ERROR]: The SSO session associated with this profile has expired or is otherwise invalid. To refresh this SSO session run aws sso login with the corresponding profile.",
			want:   store.CheckNeedsLogin,
		},
		{
			name:   "expired token code",
			stderr: "aws: [ERROR]: An error occurred (ExpiredToken): The security token included in the request is expired",
			want:   store.CheckNeedsLogin,
		},
		{
			name:   "access denied is a permissions problem, not a login one",
			stderr: "aws: [ERROR]: An error occurred (AccessDenied): User is not authorized to perform sts:GetCallerIdentity",
			want:   store.CheckMisconfigured,
		},
		{
			name:   "unknown profile",
			stderr: "aws: [ERROR]: The config profile (nope) could not be found",
			want:   store.CheckMisconfigured,
		},
		{
			name:   "endpoint unreachable is transient and must not gate",
			stderr: "aws: [ERROR]: An error occurred (EndpointConnectionError): Could not connect to the endpoint URL",
			want:   store.CheckUnavailable,
		},
		{
			name:   "dns failure is transient",
			stderr: "aws: [ERROR]: Temporary failure in name resolution",
			want:   store.CheckUnavailable,
		},
		{
			name:   "an unrecognised error must not masquerade as a dead credential",
			stderr: "aws: [ERROR]: something nobody has seen before",
			want:   store.CheckUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifyAWSError(tc.stderr, nil)
			if got != tc.want {
				t.Errorf("state = %q, want %q (detail %q)", got, tc.want, detail)
			}
			if detail == "" {
				t.Error("detail is empty; the UI would show nothing")
			}
			// The CLI's own prefix must not leak into the UI.
			if len(detail) > 6 && detail[:6] == "aws: [" {
				t.Errorf("detail still carries the CLI prefix: %q", detail)
			}
		})
	}
}

// TestGatingSemantics is the property the whole design leans on.
func TestGatingSemantics(t *testing.T) {
	gates := map[State]bool{
		store.CheckOK:            false,
		store.CheckUnavailable:   false, // transient: must never gate
		store.CheckUnknown:       false, // unrecognised: must never gate
		store.CheckNeedsLogin:    true,
		store.CheckMisconfigured: true,
	}
	for state, want := range gates {
		if got := (Result{State: state}).Gates(); got != want {
			t.Errorf("Result{%q}.Gates() = %v, want %v", state, got, want)
		}
	}
}

// TestParseProfiles covers both shapes present in the real config on this
// machine: IAM Identity Center profiles and role-chaining profiles.
func TestParseProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	content := `
[default]
region = us-east-1

[profile admin]
sso_start_url = https://example.awsapps.com/start
sso_region = us-east-1
sso_account_id = 111111111111
sso_role_name = AdministratorAccess
region = us-east-1

[profile chained]
role_arn = arn:aws:iam::222222222222:role/Target
source_profile = admin
region = us-east-1

[profile modern-sso]
sso_session = my-session
sso_account_id = 333333333333

# A session block is not a profile and must be ignored.
[sso-session my-session]
sso_start_url = https://example.awsapps.com/start
sso_region = us-east-1

[profile static]
region = eu-west-1
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := &AWSChecker{ConfigPath: path}
	profiles, err := c.parseProfiles()
	if err != nil {
		t.Fatalf("parseProfiles: %v", err)
	}

	if _, present := profiles["my-session"]; present {
		t.Error("an [sso-session] block was parsed as a profile")
	}

	want := []string{"default", "admin", "chained", "modern-sso", "static"}
	if len(profiles) != len(want) {
		t.Errorf("parsed %d profiles, want %d: %v", len(profiles), len(want), profiles)
	}

	if !profiles["admin"].IsSSO() {
		t.Error("admin should be recognised as SSO")
	}
	if !profiles["modern-sso"].IsSSO() {
		t.Error("modern-sso (sso_session form) should be recognised as SSO")
	}
	if profiles["chained"].IsSSO() {
		t.Error("chained is not itself SSO")
	}
	if profiles["chained"].SourceProfile != "admin" {
		t.Errorf("chained source_profile = %q", profiles["chained"].SourceProfile)
	}

	// A chained profile is repaired by logging in to its source.
	if got := c.LoginTarget("chained"); got != "admin" {
		t.Errorf("LoginTarget(chained) = %q, want admin", got)
	}
	if got := c.LoginTarget("admin"); got != "admin" {
		t.Errorf("LoginTarget(admin) = %q, want admin", got)
	}

	targets, err := c.Targets(context.Background())
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != len(want) {
		t.Errorf("Targets returned %d, want %d", len(targets), len(want))
	}
	// Sorted, for a stable UI ordering.
	for i := 1; i < len(targets); i++ {
		if targets[i-1] > targets[i] {
			t.Errorf("Targets not sorted: %v", targets)
			break
		}
	}
}

func TestMissingConfigIsNotAnError(t *testing.T) {
	c := &AWSChecker{ConfigPath: filepath.Join(t.TempDir(), "absent")}
	profiles, err := c.parseProfiles()
	if err != nil {
		t.Fatalf("a missing config should not error: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("got %d profiles from a missing file", len(profiles))
	}
}

func TestMissingBinaryReportsUnavailable(t *testing.T) {
	c := &AWSChecker{Binary: "", ConfigPath: filepath.Join(t.TempDir(), "absent")}
	res := c.Check(context.Background(), "admin")
	if res.State != store.CheckUnavailable {
		t.Errorf("state = %q, want unavailable", res.State)
	}
	if res.Gates() {
		t.Error("a missing aws binary must not gate; it is not a credential problem")
	}
}

func TestUnknownProfileIsMisconfigured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("[profile real]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	c := &AWSChecker{Binary: "/bin/true", ConfigPath: path}
	res := c.Check(context.Background(), "imaginary")
	if res.State != store.CheckMisconfigured {
		t.Errorf("state = %q, want misconfigured", res.State)
	}
	if !res.Gates() {
		t.Error("an unknown profile should gate: the task can never work as written")
	}
}

func TestSSORemediationTargetsTheSourceProfile(t *testing.T) {
	chained := profile{Name: "chained", RoleARN: "arn:...", SourceProfile: "admin"}
	rem := ssoRemediation(chained)
	if rem == nil || !rem.Automatic {
		t.Fatalf("remediation = %+v, want an automatic one", rem)
	}
	if rem.Command != "aws sso login --profile admin" {
		t.Errorf("command = %q, want the source profile", rem.Command)
	}

	static := profile{Name: "static"}
	rem = ssoRemediation(static)
	if rem == nil || rem.Automatic {
		t.Fatalf("a static profile cannot be fixed automatically: %+v", rem)
	}
}
