package gitops

// Helpers for reading yaml.v3 node trees, shared by the file walker and the
// splicer.

import (
	yaml "go.yaml.in/yaml/v3"
)

func contentOf(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0]
	}
	return doc
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	_, v := entry(m, key)
	return v
}

func scalarAt(m *yaml.Node, key string) string {
	v := mapValue(m, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

// entry returns a mapping's key node and value node.
func entry(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

func anchored(n *yaml.Node) bool { return n.Kind == yaml.AliasNode || n.Anchor != "" }

// isEmpty reports a key the manifest wrote and left empty: {}, null, ~ or nothing.
func isEmpty(n *yaml.Node) bool {
	return (n.Kind == yaml.MappingNode && len(n.Content) == 0) ||
		(n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

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
