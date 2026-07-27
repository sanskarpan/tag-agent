package cli_test

import (
	"strings"
	"testing"
)

// TestE2EDagRunOutOfRangeDepEchoesInteger: a DAG step dependency is a step
// INDEX, always written by the user as an integer. It lands in a
// map[string]any as a float64, and printing it with %v switched to scientific
// notation past ~1e7 — so `depends_on: [999999999]` was reported back as
// "depends on step 9.99999999e+08", which does not match anything the user
// typed. The negative case already printed correctly (-1), which is what made
// the divergence easy to miss.
func TestE2EDagRunOutOfRangeDepEchoesInteger(t *testing.T) {
	h := newHome(t)
	cases := []struct {
		name  string
		steps string
		want  string
		bad   string
	}{
		{
			name:  "large",
			steps: `[{"task":"a"},{"task":"b","depends_on":[999999999]}]`,
			want:  "depends on step 999999999",
			bad:   "e+0",
		},
		{
			name:  "negative",
			steps: `[{"task":"a"},{"task":"b","depends_on":[-1]}]`,
			want:  "depends on step -1",
		},
		{
			name:  "forward-reference",
			steps: `[{"task":"a"},{"task":"b","depends_on":[5]}]`,
			want:  "depends on step 5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if out, code := run(t, h, "dag", "save", "d-"+tc.name, "--steps", tc.steps); code != 0 {
				t.Fatalf("dag save: exit %d: %s", code, out)
			}
			out, code := run(t, h, "dag", "run", "d-"+tc.name)
			if code == 0 {
				t.Fatalf("dag run with an out-of-range dependency succeeded: %s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("dag run error = %q, want it to contain %q", strings.TrimSpace(out), tc.want)
			}
			if tc.bad != "" && strings.Contains(out, tc.bad) {
				t.Errorf("dag run error = %q, must not contain %q (scientific notation)", strings.TrimSpace(out), tc.bad)
			}
		})
	}
}

// TestE2EDagRunInvalidDepTypeIsReadable covers the sibling message on the
// `default:` branch, which formats the same `any` value.
func TestE2EDagRunInvalidDepTypeIsReadable(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "dag", "save", "dbad", "--steps",
		`[{"task":"a"},{"task":"b","depends_on":[1.5]}]`); code != 0 {
		t.Fatalf("dag save: exit %d: %s", code, out)
	}
	out, code := run(t, h, "dag", "run", "dbad")
	if code == 0 {
		t.Fatalf("dag run with a fractional dependency succeeded: %s", out)
	}
	if !strings.Contains(out, "1.5") {
		t.Errorf("dag run error = %q, want it to echo 1.5", strings.TrimSpace(out))
	}
}
