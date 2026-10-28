package llm

import "testing"

func TestParseRetrySecondsIgnoresFraction(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{in: "2", want: 2},
		{in: "1.5", want: 1},
		{in: "30.0", want: 30},
		{in: "0", want: 0},
	}
	for _, tc := range cases {
		got, err := parseRetrySeconds(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("parseRetrySeconds(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	if got, err := parseRetrySeconds("0.5"); err == nil && got > 0 {
		t.Fatalf("parseRetrySeconds(%q) = %d; a fraction under one second must not become a multi-second wait", "0.5", got)
	}
}
