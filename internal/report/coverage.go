package report

import (
	"fmt"
	"strings"
)

// Coverage counts pod containers with no series, before any display filter.
// Missing of Total had none; Dark of the Missing belong to the DarkRows rows
// in which no pod was covered, which therefore read NO-DATA. The rest belong
// to rows sized from their covered pods. Zero Total means full coverage.
//
// Pods and rows are kept apart on purpose: comparing a pod count with a
// count of NO-DATA rows, or with the rows a --top left standing, yields a
// sentence that reads well and is false.
type Coverage struct {
	Missing, Total, Dark, DarkRows int
}

// Split says where the missing pods went, naming only the parts that occur.
func (c Coverage) Split() string {
	var parts []string
	if c.Dark > 0 {
		parts = append(parts, fmt.Sprintf("%d in %d NO-DATA %s", c.Dark, c.DarkRows, plural(c.DarkRows, "row", "rows")))
	}
	if sized := c.Missing - c.Dark; sized > 0 {
		parts = append(parts, fmt.Sprintf("%d in rows sized from their other pods", sized))
	}
	return strings.Join(parts, "; ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
