package core

import (
	"regexp"
	"strings"
)

var (
	// Fenced code blocks: preserve inner code, strip delimiters and language tags.
	reFencedCode = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\n?(.*?)\n?```|~~~[a-zA-Z0-9_-]*\n?(.*?)\n?~~~")
	// Inline code: strip backticks, keep code text.
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	// Images: keep alt text, discard URL.
	reImages = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)
	// Links: keep link label, discard URL.
	reLinks = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	// Bold + italic: ***text*** or ___text___
	reBoldItalic = regexp.MustCompile(`\*{3}(.+?)\*{3}|_{3}(.+?)_{3}`)
	// Bold: **text** or __text__
	reBold = regexp.MustCompile(`\*{2}(.+?)\*{2}|_{2}(.+?)_{2}`)
	// Italic with asterisk: *text*
	reItalicStar = regexp.MustCompile(`\*(.+?)\*`)
	// Italic with underscore: only on word boundaries to preserve snake_case identifiers.
	reItalicUnd = regexp.MustCompile(`(^|\s)_([^_]+)_(\s|[.,!?;:]|$)`)
	// Strikethrough: ~~text~~
	reStrike = regexp.MustCompile(`~~(.+?)~~`)
	// ATX headers: #, ##, ### at line starts
	reHeaders = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	// Blockquotes: > at line starts
	reBlockquote = regexp.MustCompile(`(?m)^>\s?`)
	// Horizontal rules: ---, ***, ___ on a line by themselves
	reHRules = regexp.MustCompile(`(?m)^\s*[-*_]{3,}\s*\n?`)
	// Unordered list bullets: *, -, + at line starts
	reBullets = regexp.MustCompile(`(?m)^[\s]*[*\-+]\s+`)
	// HTML and XML tags: <tag>, </tag>, <tag attr="..."/>
	reHTMLTags = regexp.MustCompile(`<[^>]+>`)
	// Protocol prefixes: strip https://www. or http:// for natural spoken URLs
	reURLs = regexp.MustCompile(`https?://(www\.)?`)
	// Multiple horizontal whitespace characters collapsed to a single space
	reSpaces = regexp.MustCompile(`[ \t]+`)
	// Space immediately preceding closing punctuation
	reSpacePunct = regexp.MustCompile(`[ \t]+([,!?;:]|\.(?:\s|$))`)
	// Excess blank lines collapsed
	reMultiNewlines = regexp.MustCompile(`\n{3,}`)
)

// CleanTTSText removes markdown symbols, formatting markers, emojis, and raw tags
// from text before sending it to a text-to-speech synthesis service.
// It is idempotent: CleanTTSText(CleanTTSText(s)) == CleanTTSText(s).
func CleanTTSText(text string) string {
	if text == "" {
		return ""
	}

	// 1. Remove emojis and pictographs
	text = stripEmojis(text)

	// 2. Fenced code blocks and inline code
	text = reFencedCode.ReplaceAllString(text, "$1$2")
	text = reInlineCode.ReplaceAllString(text, "$1")

	// 3. Links and images (keep visible labels, strip targets)
	text = reImages.ReplaceAllString(text, "$1")
	text = reLinks.ReplaceAllString(text, "$1")

	// 4. Emphasis (bold, italic, strikethrough)
	text = reBoldItalic.ReplaceAllString(text, "$1$2")
	text = reBold.ReplaceAllString(text, "$1$2")
	text = reItalicStar.ReplaceAllString(text, "$1")
	for {
		next := reItalicUnd.ReplaceAllString(text, "$1$2$3")
		if next == text {
			break
		}
		text = next
	}
	text = reStrike.ReplaceAllString(text, "$1")

	// 5. Structure: headers, blockquotes, horizontal rules, bullets
	text = reHeaders.ReplaceAllString(text, "")
	text = reBlockquote.ReplaceAllString(text, "")
	text = reHRules.ReplaceAllString(text, "")
	text = reBullets.ReplaceAllString(text, "")

	// 6. HTML and XML tags
	text = reHTMLTags.ReplaceAllString(text, "")

	// 7. Simplify protocol prefixes in bare URLs
	text = reURLs.ReplaceAllString(text, "")

	// 8. Clean stray leading/trailing markers from sentence-split fragments
	text = stripStrayMarkers(text)

	// 9. Normalize whitespace and punctuation spacing
	text = reSpaces.ReplaceAllString(text, " ")
	text = reSpacePunct.ReplaceAllString(text, "$1")
	text = reMultiNewlines.ReplaceAllString(text, "\n\n")
	text = strings.TrimSpace(text)

	return text
}

// stripEmojis removes emoji runes and variation selectors.
func stripEmojis(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !isEmoji(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isEmoji reports whether r is in standard Unicode emoji and pictograph ranges.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F600 && r <= 0x1F64F: // Emoticons
		return true
	case r >= 0x1F300 && r <= 0x1F5FF: // Misc Symbols and Pictographs
		return true
	case r >= 0x1F680 && r <= 0x1F6FF: // Transport and Map
		return true
	case r >= 0x1F700 && r <= 0x1F77F: // Alchemical Symbols
		return true
	case r >= 0x1F780 && r <= 0x1F7FF: // Geometric Shapes Extended
		return true
	case r >= 0x1F800 && r <= 0x1F8FF: // Supplemental Arrows-C
		return true
	case r >= 0x1F900 && r <= 0x1F9FF: // Supplemental Symbols and Pictographs
		return true
	case r >= 0x1FA00 && r <= 0x1FA6F: // Chess Symbols
		return true
	case r >= 0x1FA70 && r <= 0x1FAFF: // Symbols and Pictographs Extended-A
		return true
	case r >= 0x2300 && r <= 0x23FF: // Misc Technical (⏰, ⌛, ⏳, etc.)
		return true
	case r >= 0x2600 && r <= 0x26FF: // Misc symbols (e.g. sun, cloud, ⚡)
		return true
	case r >= 0x2700 && r <= 0x27BF: // Dingbats (e.g. ❤️, ✨)
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // Misc Symbols and Arrows (e.g. ⭐)
		return true
	case r >= 0xFE00 && r <= 0xFE0F: // Variation selectors
		return true
	case r == 0x200D: // Zero width joiner
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // Flags (regional indicators)
		return true
	default:
		return false
	}
}

// stripStrayMarkers cleans unmatched markdown delimiters that may be left
// when a sentence tokenizer splits across formatting boundaries.
func stripStrayMarkers(s string) string {
	for {
		orig := s
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "***")
		s = strings.TrimSuffix(s, "***")
		s = strings.TrimPrefix(s, "**")
		s = strings.TrimSuffix(s, "**")
		s = strings.TrimPrefix(s, "*")
		s = strings.TrimSuffix(s, "*")
		s = strings.TrimPrefix(s, "~~")
		s = strings.TrimSuffix(s, "~~")
		s = strings.TrimPrefix(s, "`")
		s = strings.TrimSuffix(s, "`")
		s = strings.TrimSpace(s)
		if s == orig {
			break
		}
	}
	return s
}
