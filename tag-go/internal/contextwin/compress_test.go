package contextwin

import "testing"

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 0},      // 3/4 = 0
		{"abcd", 1},     // 4/4 = 1
		{"abcdefgh", 2}, // 8/4 = 2
	}
	for _, c := range cases {
		if got := EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTotalTokens(t *testing.T) {
	items := []Item{
		{Role: "user", Text: "abcd"},          // 1
		{Role: "assistant", Text: "abcdefgh"}, // 2
	}
	if got := TotalTokens(items); got != 3 {
		t.Errorf("TotalTokens = %d, want 3", got)
	}
	if got := TotalTokens(nil); got != 0 {
		t.Errorf("TotalTokens(nil) = %d, want 0", got)
	}
}

func TestKeepLast(t *testing.T) {
	items := []Item{
		{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"},
	}
	if got := KeepLast(items, 0); len(got) != 0 {
		t.Errorf("KeepLast n=0 len=%d, want 0", len(got))
	}
	if got := KeepLast(items, 2); len(got) != 2 || got[0].Text != "c" || got[1].Text != "d" {
		t.Errorf("KeepLast n=2 = %+v, want [c d]", got)
	}
	if got := KeepLast(items, 10); len(got) != 4 {
		t.Errorf("KeepLast n>len = %d, want 4", len(got))
	}
	// KeepLast must not alias the input backing array.
	got := KeepLast(items, 4)
	got[0].Text = "MUTATED"
	if items[0].Text != "a" {
		t.Error("KeepLast aliased the input slice")
	}
}

func TestTranscript(t *testing.T) {
	items := []Item{
		{Role: "user", Text: "hello"},
		{Role: "", Text: "world"}, // empty role -> "message"
	}
	got := Transcript(items)
	want := "user: hello\nmessage: world"
	if got != want {
		t.Errorf("Transcript = %q, want %q", got, want)
	}
	if got := Transcript(nil); got != "" {
		t.Errorf("Transcript(nil) = %q, want empty", got)
	}
}

func TestSummaryPrompt(t *testing.T) {
	if got := SummaryPrompt(""); got == "" {
		t.Error("SummaryPrompt(empty) should still return an instruction")
	}
	got := SummaryPrompt("user: hi")
	if len(got) == 0 || got == "user: hi" {
		t.Error("SummaryPrompt should wrap the transcript in an instruction")
	}
}
