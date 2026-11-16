package explain

import "testing"

func TestParseSignalTarget(t *testing.T) {
	cases := []struct{ target, owner, name, params string }{
		{"checkout/payment-timeouts", "checkout", "payment-timeouts", ""},
		{"checkout/topic-publishing topic=orders", "checkout", "topic-publishing", "topic=orders"},
		{"orders-db/spanner-high-cpu  z=1 instance=acme-orders", "orders-db", "spanner-high-cpu", "instance=acme-orders z=1"},
	}
	for _, tc := range cases {
		owner, name, params, err := ParseSignalTarget(tc.target)
		if err != nil || owner != tc.owner || name != tc.name || params != tc.params {
			t.Errorf("ParseSignalTarget(%q) = %q, %q, %q, %v", tc.target, owner, name, params, err)
		}
	}
	for _, bad := range []string{"", "checkout", "/cpu", "checkout/", "checkout/cpu topic", "checkout/cpu =x", "checkout/cpu k="} {
		if _, _, _, err := ParseSignalTarget(bad); err == nil {
			t.Errorf("ParseSignalTarget(%q) must fail", bad)
		}
	}
}
