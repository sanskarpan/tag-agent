// Package cron implements a hardened 5-field cron validator + matcher
// (Go port of cron_scheduler.py, with the bug-bash fixes baked in).
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var aliases = map[string][]int{ // expanded 5-field forms
	"@yearly": {0, 0, 1, 1, -1}, "@annually": {0, 0, 1, 1, -1},
	"@monthly": {0, 0, 1, -1, -1}, "@weekly": {0, 0, -1, -1, 0},
	"@daily": {0, 0, -1, -1, -1}, "@midnight": {0, 0, -1, -1, -1},
	"@hourly": {0, -1, -1, -1, -1},
}

type field struct{ lo, hi int }

var fields = []field{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
var names = []string{"minute", "hour", "day-of-month", "month", "day-of-week"}

// Validate rejects malformed exprs (negatives, */0, reversed ranges, @reboot, bad arity).
func Validate(expr string) error {
	e := strings.TrimSpace(strings.ToLower(expr))
	if e == "@reboot" {
		return fmt.Errorf("@reboot has no periodic meaning for a poller")
	}
	if _, ok := aliases[e]; ok {
		return nil
	}
	parts := strings.Fields(e)
	if len(parts) != 5 {
		return fmt.Errorf("cron expression must have exactly 5 fields, got: %q", expr)
	}
	for i, p := range parts {
		if err := validateField(p, names[i], fields[i].lo, fields[i].hi, expr); err != nil {
			return err
		}
	}
	return nil
}

func validateField(f, name string, lo, hi int, expr string) error {
	if f == "*" {
		return nil
	}
	for _, item := range strings.Split(f, ",") {
		if item == "" {
			return fmt.Errorf("empty element in cron %s of %q", name, expr)
		}
		base := item
		if strings.Contains(item, "/") {
			sp := strings.SplitN(item, "/", 2)
			base = sp[0]
			step, err := strconv.Atoi(sp[1])
			if err != nil || step <= 0 {
				return fmt.Errorf("invalid cron step %q in %s of %q", sp[1], name, expr)
			}
		}
		if base == "*" {
			continue
		}
		if strings.Contains(base, "-") {
			b := strings.Split(base, "-")
			if len(b) != 2 {
				return fmt.Errorf("invalid cron range %q in %s of %q", base, name, expr)
			}
			a, e1 := strconv.Atoi(b[0])
			c, e2 := strconv.Atoi(b[1])
			if e1 != nil || e2 != nil {
				return fmt.Errorf("invalid cron range %q in %s of %q", base, name, expr)
			}
			if a > c {
				return fmt.Errorf("reversed cron range %q in %s of %q", base, name, expr)
			}
			if a < lo || c > hi {
				return fmt.Errorf("cron %s range %q out of [%d-%d] in %q", name, base, lo, hi, expr)
			}
		} else {
			v, err := strconv.Atoi(base)
			if err != nil {
				return fmt.Errorf("invalid cron value %q in %s of %q", base, name, expr)
			}
			if v < lo || v > hi {
				return fmt.Errorf("cron %s value %d out of [%d-%d] in %q", name, v, lo, hi, expr)
			}
		}
	}
	return nil
}

// Matches reports whether t satisfies the (validated) cron expr, with POSIX
// DOM/DOW OR semantics and 0/7 both meaning Sunday.
func Matches(expr string, t time.Time) bool {
	e := strings.TrimSpace(strings.ToLower(expr))
	if a, ok := aliases[e]; ok {
		return matchAlias(a, t)
	}
	parts := strings.Fields(e)
	if len(parts) != 5 {
		return false
	}
	dow := int(t.Weekday()) // 0=Sun
	minOK := fieldMatch(parts[0], t.Minute(), fields[0].lo, fields[0].hi)
	hourOK := fieldMatch(parts[1], t.Hour(), fields[1].lo, fields[1].hi)
	monOK := fieldMatch(parts[3], int(t.Month()), fields[3].lo, fields[3].hi)
	domOK := fieldMatch(parts[2], t.Day(), fields[2].lo, fields[2].hi)
	dowOK := fieldMatch(parts[4], dow, fields[4].lo, fields[4].hi) || (dow == 0 && fieldMatch(parts[4], 7, 0, 7))
	// POSIX: if both DOM and DOW restricted -> OR; else AND
	domRestricted := parts[2] != "*"
	dowRestricted := parts[4] != "*"
	var dayOK bool
	if domRestricted && dowRestricted {
		dayOK = domOK || dowOK
	} else {
		dayOK = domOK && dowOK
	}
	return minOK && hourOK && monOK && dayOK
}

func matchAlias(a []int, t time.Time) bool {
	check := func(want, got int) bool { return want == -1 || want == got }
	dow := int(t.Weekday())
	return check(a[0], t.Minute()) && check(a[1], t.Hour()) &&
		check(a[2], t.Day()) && check(a[3], int(t.Month())) && check(a[4], dow)
}

func fieldMatch(f string, val, lo, hi int) bool {
	if f == "*" {
		return true
	}
	for _, item := range strings.Split(f, ",") {
		step := 1
		base := item
		if strings.Contains(item, "/") {
			sp := strings.SplitN(item, "/", 2)
			base = sp[0]
			s, err := strconv.Atoi(sp[1])
			if err != nil || s <= 0 {
				continue
			}
			step = s
		}
		var start, end int
		switch {
		case base == "*":
			start, end = lo, hi
		case strings.Contains(base, "-"):
			b := strings.Split(base, "-")
			start, _ = strconv.Atoi(b[0])
			end, _ = strconv.Atoi(b[1])
		default:
			n, err := strconv.Atoi(base)
			if err != nil {
				continue
			}
			if step == 1 {
				if n == val {
					return true
				}
				continue
			}
			start, end = n, hi
		}
		if val >= start && val <= end && (val-start)%step == 0 {
			return true
		}
	}
	return false
}
