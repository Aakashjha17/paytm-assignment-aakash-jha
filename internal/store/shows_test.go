package store

import "testing"

func TestSeatLabel(t *testing.T) {
	cases := []struct {
		n, perRow int
		want      string
	}{
		{1, 20, "A1"},
		{20, 20, "A20"},
		{21, 20, "B1"},
		{26 * 20, 20, "Z20"},
		{26*20 + 1, 20, "AA1"},
		{27*20 + 1, 20, "AB1"},
		{5000, 20, "IP20"},
		{1, 1, "A1"},
		{27, 1, "AA1"},
	}
	for _, c := range cases {
		if got := SeatLabel(c.n, c.perRow); got != c.want {
			t.Errorf("SeatLabel(%d, %d) = %q, want %q", c.n, c.perRow, got, c.want)
		}
	}
}

func TestSeatLabelsUnique(t *testing.T) {
	seen := map[string]bool{}
	for n := 1; n <= 50_000; n++ {
		l := SeatLabel(n, 20)
		if seen[l] {
			t.Fatalf("duplicate label %q at seat %d", l, n)
		}
		seen[l] = true
	}
}
