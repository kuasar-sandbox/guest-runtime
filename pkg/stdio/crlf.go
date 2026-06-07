package stdio

import "io"

// crlfWriter translates bare '\n' to "\r\n" on the way to w. It is used
// for everything sandbox-ctl writes to a controlling terminal that has
// been put into raw mode (its own diagnostic log, the --console=default
// kernel-dmesg passthrough) — raw mode disables the kernel's ONLCR, so
// without this such output "stairsteps". The user application's PTY
// stream is NOT wrapped: the guest-side pty already applied ONLCR.
type crlfWriter struct{ w io.Writer }

// CRLF wraps w so bare '\n' becomes "\r\n".
func CRLF(w io.Writer) io.Writer { return &crlfWriter{w: w} }

func (c *crlfWriter) Write(p []byte) (int, error) {
	// Fast path: no '\n' → write through unchanged.
	if !hasByte(p, '\n') {
		return c.w.Write(p)
	}
	var buf []byte
	last := 0
	for i, b := range p {
		if b == '\n' {
			buf = append(buf, p[last:i]...)
			buf = append(buf, '\r', '\n')
			last = i + 1
		}
	}
	buf = append(buf, p[last:]...)
	// Report len(p) consumed (the caller's byte count), not len(buf).
	if _, err := c.w.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

func hasByte(p []byte, b byte) bool {
	for _, x := range p {
		if x == b {
			return true
		}
	}
	return false
}
