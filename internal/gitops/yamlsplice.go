package gitops

import (
	"fmt"
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
	col     int // 0-based, where the value token starts
	oldText string
	newText string

	// insertAfter is set instead of col/oldText when a key is missing and new
	// lines have to be added.
	insertAfter bool
	lines       []string
}

// editRaw applies the planned changes to a file's bytes.
func editRaw(raw []byte, lookup func(kind, name, namespace string) []Change) (string, []Change, bool, error) {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			break
		}
		d := doc
		docs = append(docs, &d)
	}
	if len(docs) == 0 {
		return "", nil, false, fmt.Errorf("no documents")
	}

	lines := strings.Split(string(raw), "\n")
	var all []splice
	var applied []Change

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
		for _, c := range lookup(kind, name, scalarAt(meta, "namespace")) {
			sp, ok := spliceContainer(root, c, lines)
			if !ok {
				continue
			}
			all = append(all, sp...)
			applied = append(applied, c)
		}
	}
	if len(all) == 0 {
		return "", nil, false, nil
	}

	// Apply bottom-up so earlier line numbers stay valid.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].line != all[j].line {
			return all[i].line > all[j].line
		}
		return all[i].col > all[j].col
	})
	for _, s := range all {
		if s.insertAfter {
			lines = append(lines[:s.line+1], append(append([]string{}, s.lines...), lines[s.line+1:]...)...)
			continue
		}
		l := lines[s.line]
		if s.col > len(l) || !strings.HasPrefix(l[s.col:], s.oldText) {
			continue // the tree and the text disagree; leave the line alone
		}
		lines[s.line] = l[:s.col] + s.newText + l[s.col+len(s.oldText):]
	}
	out := strings.Join(lines, "\n")
	// The last line of defence. These edits are textual, so a tokenising
	// mistake produces a file that no longer parses, and writing that into
	// somebody's repository is the worst thing this tool could do. Re-parsing
	// costs nothing and turns a corruption into a skipped file.
	if err := parses(out); err != nil {
		return "", nil, false, fmt.Errorf("edit would not parse, file left alone: %w", err)
	}
	return out, applied, true, nil
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
		if err.Error() == "EOF" {
			return nil
		}
		return err
	}
}

// spliceContainer produces the edits for one container's resources.
func spliceContainer(root *yaml.Node, c Change, lines []string) ([]splice, bool) {
	containers := mapValue(mapValue(mapValue(mapValue(root, "spec"), "template"), "spec"), "containers")
	if containers == nil || containers.Kind != yaml.SequenceNode {
		return nil, false
	}
	for _, ctr := range containers.Content {
		if ctr.Kind != yaml.MappingNode || scalarAt(ctr, "name") != c.Container {
			continue
		}
		want := []struct {
			section string
			key     string
			val     *int64
			render  func(int64) string
		}{
			{"requests", "cpu", c.CPURequest, cpuString},
			{"requests", "memory", c.MemRequest, memString},
			{"limits", "cpu", c.CPULimit, cpuString},
			{"limits", "memory", c.MemLimit, memString},
		}
		var out []splice
		res := mapValue(ctr, "resources")
		for _, w := range want {
			if w.val == nil {
				continue
			}
			sec := mapValue(res, w.section)
			if node := mapValue(sec, w.key); node != nil && node.Kind == yaml.ScalarNode {
				if s, ok := replaceScalar(lines, node, w.render(*w.val)); ok {
					out = append(out, s)
				}
				continue
			}
			// The key is absent. Building the missing structure by text is
			// where this gets fragile, so it is done only for the whole
			// resources block at once, below.
		}
		if ins, ok := insertMissing(ctr, res, c, lines); ok {
			out = append(out, ins...)
		}
		return out, len(out) > 0
	}
	return nil, false
}

// replaceScalar rewrites a value in place, keeping any trailing comment.
func replaceScalar(lines []string, node *yaml.Node, value string) (splice, bool) {
	li := node.Line - 1
	if li < 0 || li >= len(lines) {
		return splice{}, false
	}
	col := node.Column - 1
	l := lines[li]
	if col < 0 || col > len(l) {
		return splice{}, false
	}
	old := valueToken(l[col:])
	if old == value {
		return splice{}, false
	}
	return splice{line: li, col: col, oldText: old, newText: value}, true
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
			return strings.TrimRight(rest[:i], " \t")
		case '#':
			if i > 0 && (rest[i-1] == ' ' || rest[i-1] == '\t') {
				return strings.TrimRight(rest[:i], " \t")
			}
		}
	}
	return strings.TrimRight(rest, " \t")
}

// insertMissing writes the keys the manifest does not have yet.
//
// Only three shapes are handled, which between them cover every real manifest:
// no resources block at all, a resources block missing requests, and one
// missing limits. Anything stranger is left alone rather than guessed at, and
// reported as unmatched so the omission is visible.
func insertMissing(ctr, res *yaml.Node, c Change, lines []string) ([]splice, bool) {
	needReq := (c.CPURequest != nil && mapValue(mapValue(res, "requests"), "cpu") == nil) ||
		(c.MemRequest != nil && mapValue(mapValue(res, "requests"), "memory") == nil)
	needLim := (c.CPULimit != nil && mapValue(mapValue(res, "limits"), "cpu") == nil) ||
		(c.MemLimit != nil && mapValue(mapValue(res, "limits"), "memory") == nil)
	if !needReq && !needLim {
		return nil, false
	}

	if res == nil {
		// No resources at all: append a complete block after the container's
		// last line, indented one level in from the container's own keys.
		last, indent := lastLineOf(ctr), ctr.Content[0].Column-1
		body := []string{pad(indent) + "resources:"}
		body = append(body, blockFor(indent+2, "requests", c.CPURequest, c.MemRequest)...)
		body = append(body, blockFor(indent+2, "limits", c.CPULimit, c.MemLimit)...)
		if len(body) == 1 {
			return nil, false
		}
		return []splice{{line: last, insertAfter: true, lines: body}}, true
	}

	// A resources block exists but a whole section is missing.
	indent := res.Content[0].Column - 1
	last := lastLineOf(res)
	var body []string
	if needReq && mapValue(res, "requests") == nil {
		body = append(body, blockFor(indent, "requests", c.CPURequest, c.MemRequest)...)
	}
	if needLim && mapValue(res, "limits") == nil {
		body = append(body, blockFor(indent, "limits", c.CPULimit, c.MemLimit)...)
	}
	if len(body) == 0 {
		return nil, false
	}
	return []splice{{line: last, insertAfter: true, lines: body}}, true
}

func blockFor(indent int, section string, cpu, mem *int64) []string {
	if cpu == nil && mem == nil {
		return nil
	}
	out := []string{pad(indent) + section + ":"}
	if cpu != nil {
		out = append(out, pad(indent+2)+"cpu: "+cpuString(*cpu))
	}
	if mem != nil {
		out = append(out, pad(indent+2)+"memory: "+memString(*mem))
	}
	return out
}

func pad(n int) string { return strings.Repeat(" ", n) }

// lastLineOf returns the 0-based index of the last line a node occupies.
func lastLineOf(n *yaml.Node) int {
	last := n.Line - 1
	var walk func(*yaml.Node)
	walk = func(x *yaml.Node) {
		if x.Line-1 > last {
			last = x.Line - 1
		}
		for _, ch := range x.Content {
			walk(ch)
		}
	}
	walk(n)
	return last
}
