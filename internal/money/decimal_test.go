package money

import "testing"

func TestExactRounding(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   string
	}{
		{[]string{"0.1", "0.2"}, "0.02"},
		{[]string{"1.005"}, "1.01"},
		{[]string{"-1.005"}, "-1.01"},
		{[]string{"999999.99", "3.45"}, "3449999.97"},
	} {
		got, err := Product(2, tc.values...)
		if err != nil || got != tc.want {
			t.Fatalf("%v = %s (%v), want %s", tc.values, got, err, tc.want)
		}
	}
	original, wallet, err := Charge(15, "0.60", "3.45")
	if err != nil || original != "9.00" || wallet != "31.05" {
		t.Fatalf("example = %s, %s, %v", original, wallet, err)
	}
	if _, err := Product(2, "NaN"); err == nil {
		t.Fatal("accepted invalid decimal")
	}
}
