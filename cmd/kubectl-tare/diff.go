package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/gitops"
)

// runDiff writes the findings into a checkout of the manifests, so the change
// can be reviewed, commented on and merged like any other.
//
// The report tells a platform engineer what is wrong with workloads they do not
// own. A table cannot be approved by the team that does own them, and kubectl
// patch commands leave nothing behind in the repository that is the source of
// truth. This is the output that closes that gap, and the pull request body is
// the part no other tool produces: the evidence, which is the reason a reviewer
// says yes rather than "who are you and why are you changing my deployment".
func runDiff(findings []analyze.Finding, repo string, write bool, kustomizeInto, bodyPath, source, window string) error {
	changes, skipped := gitops.Plan(findings)
	if len(changes) == 0 {
		fmt.Fprintln(os.Stderr, "no recommendation tare will stand behind; nothing to write")
		for _, s := range skipped {
			fmt.Fprintf(os.Stderr, "  skipped %s [%s]: %s\n", s.Workload, s.Container, s.Why)
		}
		return nil
	}

	if kustomizeInto != "" {
		patches, list := gitops.Kustomize(changes)
		if write {
			if err := os.MkdirAll(kustomizeInto, 0o755); err != nil {
				return err
			}
		}
		for _, p := range patches {
			path := filepath.Join(kustomizeInto, p.Filename)
			if write {
				if err := os.WriteFile(path, []byte(p.Body), 0o644); err != nil {
					return err
				}
			}
			fmt.Printf("%s %s\n", verb(write), path)
		}
		fmt.Printf("\nAdd to kustomization.yaml:\n\n%s\n", list)
	}

	if repo != "" {
		edits, missing, err := gitops.ApplyTree(repo, changes, !write)
		if err != nil {
			return err
		}
		skipped = append(skipped, missing...)
		if len(edits) == 0 {
			fmt.Fprintln(os.Stderr, "no manifest in the repository matched any recommendation")
		}
		for _, e := range edits {
			fmt.Printf("%s %s (%d container(s))\n", verb(write), e.Path, len(e.Applied))
		}
		if write {
			fmt.Printf("\n%d file(s) changed. Review with: git -C %s diff\n", len(edits), repo)
		} else {
			fmt.Printf("\n%d file(s) would change. Re-run with --write to apply, then review with git diff.\n", len(edits))
		}
	}

	body := gitops.PullRequestBody(changes, skipped, source, window)
	if bodyPath != "" {
		if err := os.WriteFile(bodyPath, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("pull request body written to %s\n", bodyPath)
	} else {
		fmt.Printf("\n--- pull request body ---\n%s", body)
	}

	// Every omission is printed, never only counted. A diff that silently
	// covers half the report invites the reviewer to believe it covers all of
	// it, and the half left behind is the half tare refused to defend.
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "\n%d finding(s) deliberately not written:\n", len(skipped))
		for _, s := range skipped {
			fmt.Fprintf(os.Stderr, "  %s [%s]: %s\n", s.Workload, s.Container, s.Why)
		}
	}
	return nil
}

func verb(write bool) string {
	if write {
		return "wrote"
	}
	return "would change"
}

// checkDiffTarget validates where diff will write, before anything is read.
func checkDiffTarget(o *options) error {
	if o.repo == "" && o.kustomize == "" {
		return fmt.Errorf("diff needs --repo <checkout> or --kustomize <dir>")
	}
	if o.repo != "" {
		return repoLooksLikeManifests(o.repo)
	}
	return nil
}

// repoLooksLikeManifests is a cheap guard against pointing the tool at the
// wrong directory and reporting that nothing matched, which is indistinguishable
// from a cluster that needs no changes.
func repoLooksLikeManifests(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("--repo %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("--repo %s is not a directory", dir)
	}
	var yamls int
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if ext := strings.ToLower(filepath.Ext(p)); ext == ".yaml" || ext == ".yml" {
				yamls++
			}
		}
		return nil
	})
	if yamls == 0 {
		return fmt.Errorf("--repo %s contains no YAML files; is it the right checkout?", dir)
	}
	return nil
}
