package util

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseSize parses a human-readable byte size such as "128MiB" or "512KB".
// Suffixes use binary units because callers use them for memory and disk sizes.
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("util: empty size")
	}
	i := 0
	for i < len(s) && (s[i] == '.' || s[i] == '-' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, unit := s[:i], strings.TrimSpace(strings.ToLower(s[i:]))
	if num == "" {
		return 0, fmt.Errorf("util: size %q missing numeric part", s)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("util: bad size %q", s)
	}
	var mult uint64
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	case "t", "tb", "tib":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("util: unknown size unit %q in %q", unit, s)
	}
	return uint64(v * float64(mult)), nil
}
