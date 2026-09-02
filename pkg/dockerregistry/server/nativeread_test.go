package server

import "testing"

func TestCompileNativeReadRegex(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		repo    string
		want    bool
	}{
		{"", "ns/app", true},
		{"ns/.*", "ns/app", true},
		{"ns/app", "ns/app-2", false},
		{"app", "ns/app", false},
	} {
		re, err := compileNativeReadRegex(tc.pattern)
		if err != nil {
			t.Fatalf("pattern %q: %v", tc.pattern, err)
		}
		if got := re.MatchString(tc.repo); got != tc.want {
			t.Errorf("pattern %q repo %q: got %v, want %v", tc.pattern, tc.repo, got, tc.want)
		}
	}
	if _, err := compileNativeReadRegex("("); err == nil {
		t.Error("expected error for invalid pattern")
	}
}
