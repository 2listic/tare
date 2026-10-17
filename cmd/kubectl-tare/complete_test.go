package main

import (
	"bytes"
	"strings"
	"testing"
)

func completeOut(t *testing.T, args ...string) (lines []string, directive string) {
	t.Helper()
	var b bytes.Buffer
	if err := runComplete(&b, args); err != nil {
		t.Fatal(err)
	}
	raw := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(raw) == 0 {
		t.Fatal("empty completion output")
	}
	return raw[:len(raw)-1], raw[len(raw)-1]
}

func TestCompleteSubcommands(t *testing.T) {
	lines, dir := completeOut(t, "")
	if dir != ":4" {
		t.Errorf("directive = %q, want :4", dir)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"report\t", "check\t", "diff\t", "version\t", "help\t"} {
		if !strings.Contains(got, want) {
			t.Errorf("subcommand completion missing %q:\n%s", want, got)
		}
	}

	lines, _ = completeOut(t, "rep")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "report\t") {
		t.Errorf("complete(rep) = %v, want [report]", lines)
	}
}

func TestCompleteFlagsPerSubcommand(t *testing.T) {
	reportFlags, _ := completeOut(t, "report", "--")
	repJoined := strings.Join(reportFlags, "\n")
	for _, want := range []string{"--demo\t", "--verdict\t", "--explain\t", "--window\t"} {
		if !strings.Contains(repJoined, want) {
			t.Errorf("report flags missing %q:\n%s", want, repJoined)
		}
	}
	for _, not := range []string{"--repo\t", "--write\t", "--kustomize\t"} {
		if strings.Contains(repJoined, not) {
			t.Errorf("report flags should not suggest diff-only %q:\n%s", not, repJoined)
		}
	}

	diffFlags, _ := completeOut(t, "diff", "--")
	diffJoined := strings.Join(diffFlags, "\n")
	for _, want := range []string{"--repo\t", "--write\t", "--kustomize\t", "--pr-body\t"} {
		if !strings.Contains(diffJoined, want) {
			t.Errorf("diff flags missing %q:\n%s", want, diffJoined)
		}
	}
	if strings.Contains(diffJoined, "--demo\t") {
		t.Errorf("diff flags should not suggest --demo:\n%s", diffJoined)
	}
}

func TestCompleteOutputFormats(t *testing.T) {
	for _, args := range [][]string{
		{"report", "-o", ""},
		{"report", "-o="},
	} {
		lines, dir := completeOut(t, args...)
		if dir != ":4" {
			t.Errorf("%v: directive = %q, want :4", args, dir)
		}
		joined := strings.Join(lines, "\n")
		for _, f := range []string{"table\t", "wide\t", "tui\t", "json\t", "html\t", "patch\t"} {
			if !strings.Contains(joined, f) {
				t.Errorf("%v: missing format %q in:\n%s", args, f, joined)
			}
		}
	}

	lines, _ := completeOut(t, "report", "-o", "w")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "wide\t") {
		t.Errorf("complete(-o w) = %v, want [wide]", lines)
	}
}

func TestCompleteVerdictsAndCommaLists(t *testing.T) {
	lines, dir := completeOut(t, "report", "--verdict", "")
	if dir != ":4" {
		t.Errorf("directive = %q, want :4", dir)
	}
	joined := strings.Join(lines, "\n")
	for _, v := range []string{"SATURATED\t", "WASTE\t", "IDLE\t", "UNVERIFIED\t", "UNSET\t", "OK\t", "NO-DATA\t"} {
		if !strings.Contains(joined, v) {
			t.Errorf("missing verdict %q in:\n%s", v, joined)
		}
	}

	// Comma-separated completion keeps the prefix and omits already-selected verdicts.
	commaLines, _ := completeOut(t, "report", "--verdict", "SATURATED,w")
	if len(commaLines) != 1 || !strings.HasPrefix(commaLines[0], "SATURATED,WASTE\t") {
		t.Errorf("complete(--verdict SATURATED,w) = %v, want [SATURATED,WASTE]", commaLines)
	}
	commaAll, _ := completeOut(t, "report", "--verdict", "SATURATED,")
	for _, l := range commaAll {
		if strings.HasPrefix(l, "SATURATED,SATURATED\t") {
			t.Errorf("already-selected verdict repeated in %v", commaAll)
		}
	}
}

func TestCompleteRepoUsesDirFilter(t *testing.T) {
	_, dir := completeOut(t, "diff", "--repo", "")
	if dir != ":16" {
		t.Errorf("diff --repo directive = %q, want :16 (ShellCompDirectiveFilterDirs)", dir)
	}
}

func TestIsCompleteInvocation(t *testing.T) {
	if rest, ok := isCompleteInvocation("/usr/local/bin/kubectl_complete-tare", []string{"report", ""}); !ok || len(rest) != 2 {
		t.Errorf("kubectl_complete-tare not recognised: ok=%v rest=%v", ok, rest)
	}
	if rest, ok := isCompleteInvocation("/usr/local/bin/kubectl-tare", []string{"__complete", "report", ""}); !ok || len(rest) != 2 {
		t.Errorf("__complete subcommand not recognised: ok=%v rest=%v", ok, rest)
	}
	if _, ok := isCompleteInvocation("/usr/local/bin/kubectl-tare", []string{"report"}); ok {
		t.Error("normal invocation misidentified as completion")
	}
}
