package dns

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fakeRunner struct {
	stdins []string
	args   [][]string
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, stdin string, args ...string) error {
	f.stdins = append(f.stdins, stdin)
	f.args = append(f.args, args)
	return f.err
}

func TestRFC2136PresentCleanUp(t *testing.T) {
	fr := &fakeRunner{}
	p := &RFC2136{Server: "ns1.example.com", TSIGName: "luncur-key", TSIGSecret: "c2VjcmV0", Runner: fr}

	if err := p.Present(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if len(fr.stdins) != 1 || fr.args[0][0] != "nsupdate" {
		t.Fatalf("runner calls: %+v", fr)
	}
	script := fr.stdins[0]
	for _, want := range []string{
		"server ns1.example.com",
		"key hmac-sha256:luncur-key c2VjcmV0", // default algo
		`update add _acme-challenge.www.example.com. 60 TXT "txtval"`,
		"send",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	// Secret never on argv.
	if strings.Contains(strings.Join(fr.args[0], " "), "c2VjcmV0") {
		t.Fatal("TSIG secret leaked to argv")
	}

	if err := p.CleanUp(context.Background(), "_acme-challenge.www.example.com", "txtval"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.stdins[1], `update delete _acme-challenge.www.example.com. TXT "txtval"`) {
		t.Fatalf("delete script:\n%s", fr.stdins[1])
	}
}

func TestRFC2136CustomAlgoAndError(t *testing.T) {
	fr := &fakeRunner{err: fmt.Errorf("SERVFAIL")}
	p := &RFC2136{Server: "ns1", TSIGName: "k", TSIGSecret: "s", TSIGAlgo: "hmac-sha512", Runner: fr}
	err := p.Present(context.Background(), "_acme-challenge.x.io", "v")
	if err == nil || !strings.Contains(err.Error(), "SERVFAIL") {
		t.Fatalf("want runner error, got %v", err)
	}
	if !strings.Contains(fr.stdins[0], "key hmac-sha512:k s") {
		t.Fatalf("custom algo missing:\n%s", fr.stdins[0])
	}
}
