package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

// helpOutput runs the real argument handling, since the flags only exist
// once run() has registered them.
func helpOutput(t *testing.T, args ...string) string {
	t.Helper()
	savedArgs, savedErr, savedSet := os.Args, os.Stderr, flag.CommandLine
	defer func() { os.Args, os.Stderr, flag.CommandLine = savedArgs, savedErr, savedSet }()
	f, err := os.CreateTemp(t.TempDir(), "help")
	if err != nil {
		t.Fatal(err)
	}
	os.Args, os.Stderr = append([]string{"kubectl-tare"}, args...), f
	flag.CommandLine = flag.NewFlagSet("kubectl-tare", flag.ContinueOnError)
	_ = run()
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

// Each command's help lists the flags it reads and none it ignores. The
// defect this pins: "diff --help" printed report's help, --repo buried in it.
func TestUsageIsPerCommand(t *testing.T) {
	for _, c := range []struct {
		cmd       string
		want, not []string
	}{
		{"report", []string{"-demo", "-o string", "-verdict"}, []string{"-repo", "-write", "-kustomize", "-pr-body"}},
		{"diff", []string{"-repo", "-write", "-kustomize", "-pr-body", "-namespace"}, []string{"-demo", "-o string", "-explain"}},
		{"check", []string{"-namespace", "-window", "-prometheus"}, []string{"-repo", "-demo", "-verdict", "-o string"}},
	} {
		out := helpOutput(t, c.cmd, "--help")
		if !strings.Contains(out, "kubectl tare "+c.cmd) {
			t.Errorf("%s help does not name its own command:\n%s", c.cmd, out)
		}
		for _, f := range c.want {
			if !strings.Contains(out, "  "+f) {
				t.Errorf("%s help is missing %s", c.cmd, f)
			}
		}
		for _, f := range c.not {
			if strings.Contains(out, "  "+f) {
				t.Errorf("%s help lists %s, which it does not read", c.cmd, f)
			}
		}
	}
}

func TestHelpCommandTakesATopic(t *testing.T) {
	if out := helpOutput(t, "help", "diff"); !strings.Contains(out, "  -repo") {
		t.Errorf("help diff did not print diff's flags:\n%s", out)
	}
}
