package cron

import (
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	good := []string{"0 2 * * *", "*/15 9-17 * * 1-5", "@daily", "1,2-4,*/10 * * * *", "0 0 1 * 1"}
	for _, e := range good {
		if err := Validate(e); err != nil {
			t.Errorf("Validate(%q) unexpected error: %v", e, err)
		}
	}
	bad := []string{"-1 0 * * *", "*/0 0 * * *", "50-10 0 * * *", "60 0 * * *", "* * *", "@reboot"}
	for _, e := range bad {
		if err := Validate(e); err == nil {
			t.Errorf("Validate(%q) expected error, got nil", e)
		}
	}
}

func TestMatchesCommaListWithRange(t *testing.T) {
	// the exact bug class the Python matcher crashed on
	tm := time.Date(2026, 7, 2, 0, 3, 0, 0, time.UTC) // minute=3
	if !Matches("1-5,10 * * * *", tm) {
		t.Error("expected minute 3 to match 1-5,10")
	}
	if Matches("1-5,10 * * * *", tm.Add(6*time.Minute)) { // minute=9
		t.Error("minute 9 should not match 1-5,10")
	}
}

func TestMatchesStep(t *testing.T) {
	tm := time.Date(2026, 7, 2, 0, 15, 0, 0, time.UTC)
	if !Matches("*/15 * * * *", tm) {
		t.Error("minute 15 should match */15")
	}
	if Matches("*/15 * * * *", tm.Add(time.Minute)) {
		t.Error("minute 16 should not match */15")
	}
}

func TestSundayZeroOrSeven(t *testing.T) {
	sun := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC) // a Sunday
	if int(sun.Weekday()) != 0 {
		t.Fatal("test fixture not a Sunday")
	}
	if !Matches("0 0 * * 7", sun) {
		t.Error("Sunday should match dow=7")
	}
	if !Matches("0 0 * * 0", sun) {
		t.Error("Sunday should match dow=0")
	}
}

func TestDomDowOrSemantics(t *testing.T) {
	// POSIX: both restricted -> OR. 1st of month OR Monday.
	firstOfMonth := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) // Wed
	if !Matches("0 0 1 * 1", firstOfMonth) {
		t.Error("1st of month should match '0 0 1 * 1' via DOM")
	}
}
