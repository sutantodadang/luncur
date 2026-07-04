package dns

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes an external command with a stdin script. Faked in tests;
// the real one shells out (nsupdate).
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) error
}

// ExecRunner runs the command for real.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdin string, args ...string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", args[0], err, bytes.TrimSpace(out))
	}
	return nil
}

// RFC2136 updates TXT records by piping an nsupdate script (server, TSIG
// key, update add/delete) to the nsupdate binary. The TSIG secret rides
// stdin's `key` line — never argv, which would be visible in `ps`.
type RFC2136 struct {
	Server     string
	TSIGName   string
	TSIGSecret string
	TSIGAlgo   string // default hmac-sha256
	Runner     Runner // default ExecRunner
}

func (p *RFC2136) algo() string {
	if p.TSIGAlgo != "" {
		return p.TSIGAlgo
	}
	return "hmac-sha256"
}

func (p *RFC2136) runner() Runner {
	if p.Runner != nil {
		return p.Runner
	}
	return ExecRunner{}
}

func (p *RFC2136) run(ctx context.Context, update string) error {
	script := "server " + p.Server + "\n" +
		"key " + p.algo() + ":" + p.TSIGName + " " + p.TSIGSecret + "\n" +
		update + "\n" +
		"send\n"
	return p.runner().Run(ctx, script, "nsupdate")
}

func (p *RFC2136) Present(ctx context.Context, fqdn, value string) error {
	return p.run(ctx, fmt.Sprintf("update add %s. 60 TXT %q", strings.TrimSuffix(fqdn, "."), value))
}

func (p *RFC2136) CleanUp(ctx context.Context, fqdn, value string) error {
	return p.run(ctx, fmt.Sprintf("update delete %s. TXT %q", strings.TrimSuffix(fqdn, "."), value))
}
