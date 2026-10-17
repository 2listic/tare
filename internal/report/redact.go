package report

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
)

// Redact replaces every cluster-specific name with a stable hash.
//
// The report is designed to be sent to someone: the person who owns a workload
// is rarely the person who ran tare. Sending it to a vendor, or attaching it to
// a bug report against this tool, is a different matter, because namespace and
// workload names describe an employer's internal systems and node names often
// carry an account id.
//
// Names are hashed rather than dropped so the report still reads as a report -
// the same workload is the same string throughout - and the same cluster hashes
// the same way every run, so two reports can be compared. The hash is stable,
// not secret: it is one sha256 of a short name, so anyone who guesses
// "production" can confirm it. This hides names from a casual reader, it does
// not withstand someone trying.
//
// Free text is rewritten too. Every reason in this report names the workload it
// is about, and a redaction that only cleaned the columns would be a promise
// the evidence section quietly broke.
func Redact(r *Report) {
	// The plot query is keyed by the names this function is about to hash.
	r.Usage, r.UsageNote = nil, "not drawn under --redact: the query needs the real names"
	secret := map[string]bool{}
	add := func(s string) {
		for _, part := range splitName(s) {
			// Two characters carry no information worth hiding and collide
			// with ordinary words; a controller kind is not a secret and
			// hashing it makes the report unreadable.
			if len(part) > 2 && !publicWord[part] {
				secret[part] = true
			}
		}
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		add(f.Namespace)
		if _, name, ok := strings.Cut(f.Workload, "/"); ok {
			add(name)
		} else {
			add(f.Workload)
		}
		add(f.Container)
		add(f.Image)
		for _, n := range f.Nodes {
			add(n)
		}
	}
	for _, c := range r.Commitment {
		add(c.Node)
	}
	for _, n := range r.Nodes.Removable {
		add(n)
	}
	for _, n := range r.Nodes.EmptyToday {
		add(n)
	}

	rep := func(s string) string {
		if s == "" {
			return s
		}
		return nameToken.ReplaceAllStringFunc(s, func(tok string) string {
			if secret[tok] {
				return hashName(tok)
			}
			return tok
		})
	}

	for i := range r.Findings {
		f := &r.Findings[i]
		f.Namespace = rep(f.Namespace)
		if kind, name, ok := strings.Cut(f.Workload, "/"); ok {
			f.Workload = kind + "/" + rep(name)
		} else {
			f.Workload = rep(f.Workload)
		}
		f.Container = rep(f.Container)
		f.Image = rep(f.Image)
		f.Nodes = repEach(rep, f.Nodes)
		f.NodeClasses = repEach(rep, f.NodeClasses)
		for j := range f.Reasons {
			f.Reasons[j].Text = rep(f.Reasons[j].Text)
			f.Reasons[j].Subject = rep(f.Reasons[j].Subject)
		}
		for j := range f.Ceilings {
			f.Ceilings[j].Note = rep(f.Ceilings[j].Note)
		}
	}
	for i := range r.Commitment {
		r.Commitment[i].Node = rep(r.Commitment[i].Node)
	}
	r.Nodes.Removable = repEach(rep, r.Nodes.Removable)
	r.Nodes.EmptyToday = repEach(rep, r.Nodes.EmptyToday)
	r.Nodes.Unusable = repEach(rep, r.Nodes.Unusable)
	r.Nodes.SelfCheck = rep(r.Nodes.SelfCheck)
	r.DrainBlockers = repEach(rep, r.DrainBlockers)
	for i := range r.Missing {
		r.Missing[i].Detail = rep(r.Missing[i].Detail)
	}
	r.NodeNote = rep(r.NodeNote)

	// The header has to say so. A redacted report that does not announce itself
	// invites a reader to go looking for a namespace called 3f2a91c4.
	// Single-spaced: the meta section is a table now, and two consecutive
	// spaces are its column separator.
	r.Source = RedactSource(rep(r.Source)) + " | names redacted"
	r.Redacted = true
}

// RedactSource hides where the data came from while keeping what kind of place
// it was. The endpoint names a namespace and a service, or a hostname, none of
// which appear anywhere else in the report and all of which describe the
// employer rather than the workload.
func RedactSource(s string) string {
	const proxy = "apiserver-proxy://"
	if rest, ok := strings.CutPrefix(s, proxy); ok {
		ns, svcPort, found := strings.Cut(rest, "/")
		if !found {
			return proxy + hashName(rest)
		}
		svc, port, hasPort := strings.Cut(svcPort, ":")
		out := proxy + hashName(ns) + "/" + hashName(svc)
		if hasPort {
			// The port name is a Prometheus convention, not a secret, and it
			// is how a reader tells a Thanos query frontend from a Prometheus.
			out += ":" + port
		}
		return out
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		host := u.Hostname()
		if port := u.Port(); port != "" {
			u.Host = hashName(host) + ":" + port
		} else {
			u.Host = hashName(host)
		}
		u.Path, u.RawQuery, u.User = "", "", nil
		return u.String()
	}
	return s
}

// nameToken matches a whole Kubernetes-style name. Replacement is by token and
// never by substring: a namespace called "sim" must not turn the word
// "simulator" into a hash followed by "ulator", which is what a plain
// search-and-replace does.
var nameToken = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._-]*`)

// splitName breaks a compound identifier - "prod/Deployment/web",
// "registry.example.com/team/api:1.4" - into the parts worth hiding.
func splitName(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '/' || r == ':' || r == '@' || r == ' '
	})
}

// publicWord is the vocabulary the report needs in order to still mean
// something: controller kinds are not anyone's secret.
var publicWord = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true,
	"ReplicationController": true, "CronJob": true, "Job": true, "Pod": true,
	"latest": true,
}

func repEach(rep func(string) string, in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, rep(s))
	}
	return out
}

func hashName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// HashName returns the stable 8-character hash Redact uses for a cluster name.
func HashName(s string) string {
	return hashName(s)
}
