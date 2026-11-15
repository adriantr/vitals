package report

import (
	"fmt"
	"math"
)

// Format prints a value in its unit, compactly.
func Format(v float64, unit string) string {
	switch {
	case math.IsNaN(v):
		return "n/a"
	case unit == "ms":
		return duration(v)
	case unit == "s":
		return duration(v * 1000)
	case unit == "bytes":
		for _, u := range []string{"B", "KiB", "MiB", "GiB"} {
			if v < 1024 || u == "GiB" {
				return fmt.Sprintf("%.1f%s", v, u)
			}
			v /= 1024
		}
	case unit == "percent":
		switch pct := v * 100; {
		case pct > 0 && pct < 1:
			return fmt.Sprintf("%.2f%%", pct)
		case pct < 10:
			return fmt.Sprintf("%.1f%%", pct)
		default:
			return fmt.Sprintf("%.0f%%", pct)
		}
	}
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func duration(ms float64) string {
	switch {
	case ms >= 60_000:
		return fmt.Sprintf("%.1fmin", ms/60_000)
	case ms >= 1000:
		return fmt.Sprintf("%.2fs", ms/1000)
	default:
		return fmt.Sprintf("%.0fms", ms)
	}
}

// Optional formats a measure that may be missing.
func Optional(v *float64, unit string) string {
	if v == nil {
		return "—"
	}
	return Format(*v, unit)
}
