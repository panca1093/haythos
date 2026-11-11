package task

import "strings"

// StripInvisible removes zero-width, bidi, Unicode tag and filler characters, which can hide instructions from a human reader.
// Emoji selectors, soft hyphens and the combining grapheme joiner stay: ordinary text uses them.
func StripInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E,
			r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069, r == 0xFEFF,
			r >= 0xE0000 && r <= 0xE007F, r >= 0xE0100 && r <= 0xE01EF,
			r == 0x115F, r == 0x1160, r == 0x3164, r == 0xFFA0, r == 0x180E:
			return -1
		}
		return r
	}, s)
}
