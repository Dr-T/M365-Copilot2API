package chathub

import "testing"

func TestUnseenSuffix(t *testing.T) {
	cases := []struct {
		name     string
		snapshot string
		cur      string
		want     string
	}{
		{"empty snapshot", "", "abc", ""},
		{"empty stream", "abc", "", "abc"},
		{"pure append", "abcdef", "abc", "def"},
		{"cur inside snapshot", "xyzabcdef", "abc", "def"},
		{"snapshot inside cur", "abc", "xxabcyy", ""},
		{"snapshot is cur prefix", "abc", "abcdef", ""},
		{"snapshot ends with cur", "newheadabc", "abc", "newhead"},
		{"full rewrite no overlap", "xyz", "abc", "xyz"},
		{"mid-stream rewrite", "abXcdef", "abcdef", "Xcdef"},
		{"identical", "abc", "abc", ""},
		{"crlf vs lf same content", "a\r\nb", "a\nb", ""},
		{"crlf inside stream", "line1\r\nline2ext", "line1\nline2", ""},
		{"zero-width diff", "a\u200bb", "ab", ""},
		{"punct edit mid-stream", "abcXYdef", "abcdef", "XYdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unseenSuffix(tc.snapshot, tc.cur); got != tc.want {
				t.Errorf("unseenSuffix(%q, %q) = %q, want %q", tc.snapshot, tc.cur, got, tc.want)
			}
		})
	}
}