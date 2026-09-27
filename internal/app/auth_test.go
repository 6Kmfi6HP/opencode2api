package app

import "testing"

// Regression: isValidOpenCodeKey must accept oc_sk- / oc_sk_ 前缀（真实发行格
// 式之一），同时拒绝 sk-ant- / 占位短串。
func TestIsValidOpenCodeKeyOcSk(t *testing.T) {
	cases := []struct {
		token string
		want  bool
	}{
		{"oc_sk_61bcccca210d_FNPhvD5AIcGQyVk1nNPpPVjeebvek69i", true},
		{"oc_sk-abcdef1234567890", true},
		{"sk-abcdefghijklmnopq", true},
		{"sk-ant-abcdefghijk", false},
		{"sk-", false},
		{"oc_sk-", false},
		{"public", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isValidOpenCodeKey(c.token); got != c.want {
			t.Errorf("isValidOpenCodeKey(%q) = %v, want %v", c.token, got, c.want)
		}
	}
}
