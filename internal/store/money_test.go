package store

import "testing"

func TestPercentageDiscountRoundsHalfUp(t *testing.T) {
	tests := []struct {
		name     string
		subtotal int64
		percent  int
		want     int64
	}{
		{name: "exact", subtotal: 1000, percent: 10, want: 100},
		{name: "round down", subtotal: 104, percent: 10, want: 10},
		{name: "half rounds up", subtotal: 105, percent: 10, want: 11},
		{name: "full discount capped", subtotal: 99, percent: 100, want: 99},
		{name: "zero subtotal", subtotal: 0, percent: 10, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := percentageDiscount(test.subtotal, test.percent); got != test.want {
				t.Fatalf("percentageDiscount(%d, %d) = %d, want %d", test.subtotal, test.percent, got, test.want)
			}
		})
	}
}
