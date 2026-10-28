package task

import "testing"

func TestStripInvisible(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "hello dunia", "hello dunia"},
		{"keeps emoji and accents", "café 👍", "café 👍"},
		{"zero width", "a\u200bb\u200cc\u200dd", "abcd"},
		{"marks", "a\u200eb\u200fc", "abc"},
		{"bidi embeddings", "a\u202ab\u202bc\u202cd\u202de\u202ef", "abcdef"},
		{"word joiner range", "a\u2060b\u2061c\u2062d\u2063e\u2064f", "abcdef"},
		{"isolates", "a\u2066b\u2067c\u2068d\u2069e", "abcde"},
		{"bom", "\ufeffstart", "start"},
		{"boundaries kept", " |\u2065|\u206a| ", " |\u2065|\u206a| "},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripInvisible(tc.in); got != tc.want {
				t.Fatalf("StripInvisible(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
