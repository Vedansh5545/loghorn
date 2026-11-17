package adapter

import "time"

// StripActionsPrefix removes the per-line timestamp GitHub Actions puts in
// front of every job-log line (`2026-11-17T17:00:30.3832868Z `): four-digit
// year through a literal Z, an optional 1-9 digit fraction, then one space.
// ok is false, and rest is line, when the line has no such prefix.
func StripActionsPrefix(line []byte) (rest []byte, t time.Time, ok bool) {
	i := 0
	digits := func(n int) bool {
		if i+n > len(line) {
			return false
		}
		for j := 0; j < n; j++ {
			if c := line[i+j]; c < '0' || c > '9' {
				return false
			}
		}
		i += n
		return true
	}
	lit := func(c byte) bool {
		if i < len(line) && line[i] == c {
			i++
			return true
		}
		return false
	}
	if !(digits(4) && lit('-') && digits(2) && lit('-') && digits(2) && lit('T') &&
		digits(2) && lit(':') && digits(2) && lit(':') && digits(2)) {
		return line, time.Time{}, false
	}
	if i < len(line) && line[i] == '.' {
		i++
		n := 0
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			i++
			n++
		}
		if n < 1 || n > 9 {
			return line, time.Time{}, false
		}
	}
	if !lit('Z') || !lit(' ') {
		return line, time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, string(line[:i-1]))
	if err != nil {
		return line, time.Time{}, false
	}
	return line[i:], ts, true
}
