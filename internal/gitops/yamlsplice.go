package gitops

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Re-encoding a YAML document is not an option, which the first version of this
// package learned the expensive way.
//
// yaml.Node preserves comments and key order, so a decode/encode round trip
// looked correct. It is not: the encoder normalises sequence indentation, so a
// one-container change reformatted every list in the file and produced a
// 117-line diff for a two-line edit. A reviewer presented with that rejects it
// without reading it, and the entire value of this output is that somebody
// reads it.
//
// So the tree is parsed only to find things. The bytes are edited in place:
// every line the change does not touch comes out identical, and `git diff`
// shows exactly the resource values and nothing else.

// splice is one pending modification to the raw text.
type splice struct {
	line    int // 0-based
	col     int // 0-based byte offset, where the value token starts
	oldText string
	newText string

	// insertAfter is set instead of col/oldText when a key is missing and new
	// lines have to be added.
	insertAfter bool
	lines       []string

	owner int // index into the document's changes, so a failed splice un-applies its change
}

// refusal is a change whose manifest was found but deliberately not edited.
// It is reported with its reason: "no manifest found" would send the reader
// looking for a Helm chart that does not exist.
type refusal struct {
	c   Change
	why string
}

// editRaw applies the planned changes to a file's bytes.
//
// applied holds every change the file now satisfies, including one whose
// values were already there; changed reports whether the bytes differ. On an
// error, applied holds the changes that were not made because of it.
func editRaw(raw []byte, lookup func(kind, name, namespace string) ([]Change, []refusal)) (out string, applied []Change, refused []refusal, changed bool, err error) {
	docs := parseDocs(raw)
	if len(docs) == 0 {
		return "", nil, nil, false, fmt.Errorf("no documents")
	}
	lines, eol := splitLines(raw)

	var all []splice
	var cands []Change
	for _, doc := range docs {
		root := contentOf(doc)
		if root == nil || root.Kind != yaml.MappingNode {
			continue
		}
		kind := scalarAt(root, "kind")
		meta := mapValue(root, "metadata")
		name := scalarAt(meta, "name")
		if kind == "" || name == "" {
			continue
		}
		want, no := lookup(kind, name, scalarAt(meta, "namespace"))
		refused = append(refused, no...)
		want, no = oneChangePerContainer(want)
		refused = append(refused, no...)
		for _, c := range want {
			sp, why, found := spliceContainer(root, c, lines)
			if !found {
				continue
			}
			if why != "" {
				refused = append(refused, refusal{c, why})
				continue
			}
			for i := range sp {
				sp[i].owner = len(cands)
			}
			all = append(all, sp...)
			cands = append(cands, c)
		}
	}
	if len(all) == 0 {
		return "", cands, refused, false, nil
	}

	lines, failed := applyWhole(lines, all)
	for i, c := range cands {
		if failed[i] {
			refused = append(refused, refusal{c, "the manifest text did not match its parsed structure, so the edit was not made"})
		} else {
			applied = append(applied, c)
		}
	}
	out = strings.Join(lines, eol)
	// The last line of defence. These edits are textual, so a tokenising
	// mistake produces a file that no longer parses, and writing that into
	// somebody's repository is the worst thing this tool could do. Re-parsing
	// costs nothing and turns a corruption into a skipped file.
	if err := parses(out); err != nil {
		return "", applied, refused, false, fmt.Errorf("edit would not parse, file left alone: %w", err)
	}
	return out, applied, refused, out != string(raw), nil
}

const whyDuplicate = "more than one recommendation targets this container, and tare will not choose between them"

// oneChangePerContainer refuses every change to a container that more than one
// change targets. Each would be planned against the same original bytes, so
// both replaced one value token ("16MiMi") or both added a resources block,
// and the result still parsed.
func oneChangePerContainer(want []Change) (kept []Change, refused []refusal) {
	n := map[string]int{}
	for _, c := range want {
		n[c.Container]++
	}
	for _, c := range want {
		if n[c.Container] > 1 {
			refused = append(refused, refusal{c, whyDuplicate})
			continue
		}
		kept = append(kept, c)
	}
	return kept, refused
}

// parseDocs decodes every document in a file, stopping at the first that does
// not parse.
func parseDocs(raw []byte) []*yaml.Node {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			return docs
		}
		d := doc
		docs = append(docs, &d)
	}
}

// splitLines splits a file into lines and reports its line ending. A file
// checked out with CRLF endings keeps them. Splitting on \n alone left a \r on
// every value token, which the replacement then dropped.
func splitLines(raw []byte) (lines []string, eol string) {
	text := string(raw)
	eol = "\n"
	if n := strings.Count(text, "\n"); n > 0 && strings.Count(text, "\r\n") == n {
		eol = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	return strings.Split(text, "\n"), eol
}

// mergeInserts joins inserts after the same line in the order they were
// planned: applied one by one, a later block would land above an earlier one,
// and a key meant for requests would end up under limits.
func mergeInserts(all []splice) []splice {
	var merged []splice
	at := map[int]int{}
	for _, s := range all {
		if !s.insertAfter {
			merged = append(merged, s)
			continue
		}
		if i, ok := at[s.line]; ok {
			merged[i].lines = append(merged[i].lines, s.lines...)
			continue
		}
		at[s.line] = len(merged)
		s.lines = append([]string{}, s.lines...)
		merged = append(merged, s)
	}
	return merged
}

// applyWhole applies every change whose splices all match the text, and none
// of a change that has one that does not. It reports the owners left out.
func applyWhole(lines []string, all []splice) ([]string, map[int]bool) {
	failed := mismatched(lines, all)
	return applySplices(lines, mergeInserts(withoutOwners(all, failed))), failed
}

// mismatched reports the owners of any splice whose text does not match the
// parsed structure. It runs before anything is applied: a change is several
// splices, and applying the ones that matched while refusing the change wrote
// half an edit into the file and reported none.
func mismatched(lines []string, all []splice) map[int]bool {
	failed := map[int]bool{}
	var replaces []splice
	for _, s := range all {
		if s.insertAfter {
			continue
		}
		if s.line >= len(lines) {
			failed[s.owner] = true
			continue
		}
		l := lines[s.line]
		if s.col > len(l) || !strings.HasPrefix(l[s.col:], s.oldText) {
			failed[s.owner] = true
			continue
		}
		replaces = append(replaces, s)
	}
	// Two replacements over the same bytes were both checked against the
	// original text, so both pass; applied, the second lands inside the first.
	sort.SliceStable(replaces, func(i, j int) bool {
		if replaces[i].line != replaces[j].line {
			return replaces[i].line < replaces[j].line
		}
		return replaces[i].col < replaces[j].col
	})
	for i := 1; i < len(replaces); i++ {
		a, b := replaces[i-1], replaces[i]
		if a.line == b.line && b.col < a.col+len(a.oldText) {
			failed[a.owner], failed[b.owner] = true, true
		}
	}
	return failed
}

// withoutOwners drops every splice that belongs to a failed change.
func withoutOwners(all []splice, failed map[int]bool) []splice {
	var kept []splice
	for _, s := range all {
		if !failed[s.owner] {
			kept = append(kept, s)
		}
	}
	return kept
}

// applySplices applies bottom-up so earlier line numbers stay valid. Every
// splice has already been checked against the text by mismatched.
func applySplices(lines []string, merged []splice) []string {
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].line != merged[j].line {
			return merged[i].line > merged[j].line
		}
		return merged[i].col > merged[j].col
	})
	for _, s := range merged {
		if s.insertAfter {
			lines = append(lines[:s.line+1], append(append([]string{}, s.lines...), lines[s.line+1:]...)...)
			continue
		}
		l := lines[s.line]
		lines[s.line] = l[:s.col] + s.newText + l[s.col+len(s.oldText):]
	}
	return lines
}

// parses reports whether every document in a stream still decodes.
func parses(s string) error {
	dec := yaml.NewDecoder(strings.NewReader(s))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

const (
	whyAnchor = "its resources are shared through a YAML anchor or alias, so editing them here would change every container that references them"
	whyMerge  = "the container is built from a YAML merge key (<<), so its resources are inherited and a block added here would override them"
	whyFlow   = "its resources are written in flow style ({...}) without the key to change, and tare only adds keys to block-style YAML"
	whyShape  = "its resources are not a plain mapping tare can edit"
)

// spliceContainer produces the edits for one container's resources.
//
// found is false when the document has no such container. When it does, the
// result is either the splices (possibly none, when the values are already
// there) or a reason the container was left alone. Every shape not handled
// here is refused with a reason rather than guessed at: a guess that parses is
// a silent wrong edit, and the parse check cannot catch it.
func spliceContainer(root *yaml.Node, c Change, lines []string) (out []splice, why string, found bool) {
	containers := mapValue(mapValue(mapValue(mapValue(root, "spec"), "template"), "spec"), "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return nil, "", false
	}
	for _, ctr := range containers.Content {
		if ctr.Kind != yaml.MappingNode || scalarAt(ctr, "name") != c.Container {
			continue
		}
		if mapValue(ctr, "<<") != nil {
			return nil, whyMerge, true
		}
		if containers.Anchor != "" || ctr.Anchor != "" {
			return nil, whyAnchor, true
		}

		resKey, res := entry(ctr, "resources")
		if res == nil {
			if ctr.Style&yaml.FlowStyle != 0 {
				return nil, whyFlow, true
			}
			// No resources at all: append a complete block after the
			// container's last line, at the indent of its own keys.
			indent := ctr.Content[0].Column - 1
			body := []string{pad(indent) + "resources:"}
			body = append(body, blockFor(indent+2, "requests", c.CPURequest, c.MemRequest)...)
			body = append(body, blockFor(indent+2, "limits", c.CPULimit, c.MemLimit)...)
			if len(body) == 1 {
				return nil, "", true
			}
			return []splice{{line: lastLineOf(ctr), insertAfter: true, lines: body}}, "", true
		}
		if anchored(res) {
			return nil, whyAnchor, true
		}
		if isEmpty(res) {
			// `resources: {}` is what most chart scaffolds render.
			var body []string
			body = append(body, blockFor(resKey.Column+1, "requests", c.CPURequest, c.MemRequest)...)
			body = append(body, blockFor(resKey.Column+1, "limits", c.CPULimit, c.MemLimit)...)
			return clearAndFill(resKey, lines, body)
		}
		if res.Kind != yaml.MappingNode {
			return nil, whyShape, true
		}

		var missingSections []string
		for _, sec := range []struct {
			name     string
			cpu, mem *int64
		}{
			{"requests", c.CPURequest, c.MemRequest},
			{"limits", c.CPULimit, c.MemLimit},
		} {
			if sec.cpu == nil && sec.mem == nil {
				continue
			}
			secKey, node := entry(res, sec.name)
			if node == nil {
				if res.Style&yaml.FlowStyle != 0 {
					return nil, whyFlow, true
				}
				missingSections = append(missingSections, blockFor(res.Content[0].Column-1, sec.name, sec.cpu, sec.mem)...)
				continue
			}
			if anchored(node) {
				return nil, whyAnchor, true
			}
			if isEmpty(node) {
				if res.Style&yaml.FlowStyle != 0 {
					return nil, whyFlow, true
				}
				sp, why, _ := clearAndFill(secKey, lines, keysFor(secKey.Column+1, sec.cpu, sec.mem))
				if why != "" {
					return nil, why, true
				}
				out = append(out, sp...)
				continue
			}
			if node.Kind != yaml.MappingNode {
				return nil, whyShape, true
			}
			var missCPU, missMem *int64
			for _, k := range []struct {
				key    string
				val    *int64
				render func(int64) string
				miss   **int64
			}{
				{"cpu", sec.cpu, CPUQuantity, &missCPU},
				{"memory", sec.mem, MemQuantity, &missMem},
			} {
				if k.val == nil {
					continue
				}
				v := mapValue(node, k.key)
				if v == nil {
					*k.miss = k.val
					continue
				}
				if anchored(v) {
					return nil, whyAnchor, true
				}
				if v.Kind != yaml.ScalarNode {
					return nil, whyShape, true
				}
				sp, ok, err := replaceScalar(lines, v, k.render(*k.val))
				if err != "" {
					return nil, err, true
				}
				if ok {
					out = append(out, sp)
				}
			}
			if missCPU != nil || missMem != nil {
				if node.Style&yaml.FlowStyle != 0 {
					return nil, whyFlow, true
				}
				out = append(out, splice{line: lastLineOf(node), insertAfter: true,
					lines: keysFor(node.Content[0].Column-1, missCPU, missMem)})
			}
		}
		if len(missingSections) > 0 {
			out = append(out, splice{line: lastLineOf(res), insertAfter: true, lines: missingSections})
		}
		return out, "", true
	}
	return nil, "", false
}

// clearAndFill turns `key: {}` (or null, or nothing) into `key:` followed by
// body. It works on the key's own line, so it refuses anything but a plain key
// whose empty value sits beside it.
func clearAndFill(key *yaml.Node, lines []string, body []string) ([]splice, string, bool) {
	if len(body) == 0 {
		return nil, "", true
	}
	li := key.Line - 1
	if li < 0 || li >= len(lines) {
		return nil, whyShape, true
	}
	l := lines[li]
	b, ok := byteCol(l, key.Column)
	if !ok || !strings.HasPrefix(l[b:], key.Value+":") {
		return nil, whyShape, true
	}
	after := b + len(key.Value) + 1
	rest := l[after:]
	trimmed := strings.TrimLeft(rest, " \t")
	tok := valueToken(trimmed) // which stops at the brace and would read {} as {
	if strings.HasPrefix(trimmed, "{}") {
		tok = "{}"
	}
	switch tok {
	case "", "{}", "null", "Null", "NULL", "~":
	default:
		return nil, whyShape, true
	}
	out := []splice{{line: li, insertAfter: true, lines: body}}
	if tok != "" {
		out = append(out, splice{line: li, col: after, oldText: rest[:len(rest)-len(trimmed)+len(tok)], newText: ""})
	}
	return out, "", true
}

// replaceScalar rewrites a value in place, keeping any trailing comment.
func replaceScalar(lines []string, node *yaml.Node, value string) (splice, bool, string) {
	li := node.Line - 1
	if li < 0 || li >= len(lines) {
		return splice{}, false, whyShape
	}
	l := lines[li]
	col, ok := byteCol(l, node.Column)
	if !ok {
		return splice{}, false, whyShape
	}
	old := valueToken(l[col:])
	if old == value {
		return splice{}, false, ""
	}
	return splice{line: li, col: col, oldText: old, newText: value}, true, ""
}

// byteCol converts yaml.v3's 1-based column, which counts characters, into a
// byte offset into the line. Slicing by the column directly landed two bytes
// early after an "é" and wrote a bare key that still parsed.
func byteCol(line string, column int) (int, bool) {
	n := 0
	for i := range line {
		if n == column-1 {
			return i, true
		}
		n++
	}
	if n == column-1 {
		return len(line), true
	}
	return 0, false
}

// valueToken returns just the scalar at the start of rest.
//
// Getting this wrong corrupts the file. The first version ran to the end of the
// line or to a trailing comment, which is correct for block style and wrong for
// flow style: on `limits: {cpu: "2", memory: 1Gi}` it swallowed the closing
// brace and wrote YAML that no longer parses. Real manifests use both styles,
// and the lab's did.
func valueToken(rest string) string {
	if rest == "" {
		return ""
	}
	if q := rest[0]; q == '"' || q == '\'' {
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' && q == '"' {
				i++
				continue
			}
			if rest[i] == q {
				return rest[:i+1]
			}
		}
		return rest // unterminated; the parse check below will reject the edit
	}
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case ',', '}', ']':
			return strings.TrimRight(rest[:i], " \t\r")
		case '#':
			if i > 0 && (rest[i-1] == ' ' || rest[i-1] == '\t') {
				return strings.TrimRight(rest[:i], " \t\r")
			}
		}
	}
	// A stray \r survives in a file with mixed line endings.
	return strings.TrimRight(rest, " \t\r")
}

func blockFor(indent int, section string, cpu, mem *int64) []string {
	if cpu == nil && mem == nil {
		return nil
	}
	return append([]string{pad(indent) + section + ":"}, keysFor(indent+2, cpu, mem)...)
}

func keysFor(indent int, cpu, mem *int64) []string {
	var out []string
	if cpu != nil {
		out = append(out, pad(indent)+"cpu: "+CPUQuantity(*cpu))
	}
	if mem != nil {
		out = append(out, pad(indent)+"memory: "+MemQuantity(*mem))
	}
	return out
}

func pad(n int) string { return strings.Repeat(" ", n) }
