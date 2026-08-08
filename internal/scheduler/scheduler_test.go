package scheduler

import "testing"

func TestParseStatuses(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int
	}{{"[200,204,301]", 3}, {"200,204", 2}, {"", 2}} {
		if values := parseStatuses(test.raw); len(values) != test.want {
			t.Fatalf("parseStatuses(%q): expected %d values, got %#v", test.raw, test.want, values)
		}
	}
}
