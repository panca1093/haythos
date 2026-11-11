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
		{"unicode tag characters", "a\U000E0041\U000E0042\U000E007Fb", "ab"},
		{"variation selector supplement", "a\U000E0100b\U000E01EFc", "abc"},
		{"fillers and vowel separator", "a\u115fb\u1160c\u3164d\uffa0e\u180ef", "abcdef"},
		{"keeps emoji selector soft hyphen and joiner", "\u2764\ufe0f co\u00adop \u034f", "\u2764\ufe0f co\u00adop \u034f"},
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
