package review

import "testing"

// TestReplyLineBreaksCoversEveryBreakCodePoint checks the replacer against the
// code points themselves rather than against literals in a source file, so a
// substituted or corrupted invisible character cannot pass unnoticed.
func TestReplyLineBreaksCoversEveryBreakCodePoint(t *testing.T) {
	for _, codePoint := range []rune{
		0x000A, // line feed, already a newline
		0x000B, // vertical tab
		0x000C, // form feed
		0x000D, // carriage return
		0x0085, // next line
		0x2028, // line separator
		0x2029, // paragraph separator
	} {
		body := "before" + string(codePoint) + "after"
		got := replyLineBreaks.Replace(body)
		if got != "before\nafter" {
			t.Fatalf("U+%04X: replaced to %q, want the break reduced to a newline", codePoint, got)
		}
	}

	// The named constants must be exactly the code points they claim to be.
	for _, constant := range []struct {
		name  string
		value string
		want  rune
	}{
		{name: "nextLine", value: nextLine, want: 0x0085},
		{name: "lineSeparator", value: lineSeparator, want: 0x2028},
		{name: "paragraphSeparator", value: paragraphSeparator, want: 0x2029},
	} {
		runes := []rune(constant.value)
		if len(runes) != 1 || runes[0] != constant.want {
			t.Fatalf("%s = %q (%U), want exactly %U", constant.name, constant.value, runes, constant.want)
		}
	}
}
