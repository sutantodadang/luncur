package cli

import (
	"strings"
	"testing"
)

func TestWebhookCommands(t *testing.T) {
	srv := testEnv(t)

	if _, err := run(t, "login", srv.URL, "--email", "root@b.co", "--password", "pw123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "project", "create", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "app", "create", "g", "--project", "proj", "--port", "8080", "--git-url", "https://x/y.git"); err != nil {
		t.Fatal(err)
	}

	// Show before enable: disabled.
	out, err := run(t, "webhook", "show", "g", "--project", "proj")
	if err != nil {
		t.Fatalf("webhook show: %v (%s)", err, out)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("want disabled, got %q", out)
	}

	// Enable: prints the full URL, secret, one-time notice, provider hints.
	out, err = run(t, "webhook", "enable", "g", "--project", "proj")
	if err != nil {
		t.Fatalf("webhook enable: %v (%s)", err, out)
	}
	if !strings.Contains(out, srv.URL+"/hooks/apps/proj/g") {
		t.Fatalf("want full URL in output, got %q", out)
	}
	if !strings.Contains(out, "secret: ") {
		t.Fatalf("want secret in output, got %q", out)
	}
	if !strings.Contains(out, "shown once") {
		t.Fatalf("want one-time notice, got %q", out)
	}
	if !strings.Contains(out, "GitHub") || !strings.Contains(out, "GitLab") {
		t.Fatalf("want provider hints, got %q", out)
	}

	// Extract the secret line to prove rotation changes it.
	firstSecretLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "secret: ") {
			firstSecretLine = line
		}
	}
	if firstSecretLine == "" {
		t.Fatalf("no secret line found in %q", out)
	}

	// Show after enable: enabled + URL, secret masked.
	out, err = run(t, "webhook", "show", "g", "--project", "proj")
	if err != nil {
		t.Fatalf("webhook show after enable: %v (%s)", err, out)
	}
	if !strings.Contains(out, "enabled") || !strings.Contains(out, srv.URL+"/hooks/apps/proj/g") {
		t.Fatalf("want enabled + URL, got %q", out)
	}
	if !strings.Contains(out, "(set)") {
		t.Fatalf("want masked secret notice, got %q", out)
	}

	// Rotate: enabling again prints a different secret line.
	out, err = run(t, "webhook", "enable", "g", "--project", "proj")
	if err != nil {
		t.Fatalf("webhook re-enable: %v (%s)", err, out)
	}
	rotatedSecretLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "secret: ") {
			rotatedSecretLine = line
		}
	}
	if rotatedSecretLine == "" || rotatedSecretLine == firstSecretLine {
		t.Fatalf("want rotated secret, got %q (was %q)", rotatedSecretLine, firstSecretLine)
	}

	// Disable.
	out, err = run(t, "webhook", "disable", "g", "--project", "proj")
	if err != nil {
		t.Fatalf("webhook disable: %v (%s)", err, out)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("want disabled, got %q", out)
	}
	out, err = run(t, "webhook", "show", "g", "--project", "proj")
	if err != nil {
		t.Fatalf("webhook show after disable: %v (%s)", err, out)
	}
	if !strings.Contains(out, "disabled") {
		t.Fatalf("want disabled after disable, got %q", out)
	}

	// Non-git app: enable fails.
	if _, err := run(t, "app", "create", "tb", "--project", "proj", "--port", "8080"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "webhook", "enable", "tb", "--project", "proj"); err == nil {
		t.Fatal("enable on non-git app should fail")
	}
}
