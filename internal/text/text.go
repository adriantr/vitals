// Package text holds small formatting helpers shared by the renderers.
package text

import "strings"

// List joins items with commas, or prints a dash for none.
func List(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ", ")
}
