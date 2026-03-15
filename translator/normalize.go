package main

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var multiSpaceRe = regexp.MustCompile(`\s+`)

// normalizeForCache canonicalizes a Cyrillic text string for cache keying.
// Cyrillic is case-sensitive for translation (е vs Е changes meaning),
// so we do NOT lowercase. We normalize:
//   - Unicode NFC normalization (composed form)
//   - Trim leading/trailing whitespace
//   - Collapse runs of whitespace to a single space
//   - Normalize quote characters to ASCII equivalents
//   - Strip zero-width and soft-hyphen characters
func normalizeForCache(s string) string {
	// Unicode NFC
	s = norm.NFC.String(s)

	// Strip zero-width chars and soft hyphens
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff', '\u00ad':
			return -1 // drop
		default:
			return r
		}
	}, s)

	// Normalize quotes
	s = normalizeQuotes(s)

	// Trim and collapse whitespace
	s = strings.TrimSpace(s)
	s = multiSpaceRe.ReplaceAllString(s, " ")

	// Trim trailing punctuation-only noise (but keep if the whole string is punctuation)
	trimmed := strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsSpace(r)
	})
	if trimmed != "" {
		s = trimmed
	}

	return s
}

func normalizeQuotes(s string) string {
	replacer := strings.NewReplacer(
		"\u201c", "\"", // left double
		"\u201d", "\"", // right double
		"\u201e", "\"", // double low-9
		"\u00ab", "\"", // left guillemet
		"\u00bb", "\"", // right guillemet
		"\u2018", "'",  // left single
		"\u2019", "'",  // right single
		"\u201a", "'",  // single low-9
		"\u2039", "'",  // left single guillemet
		"\u203a", "'",  // right single guillemet
		"\u2010", "-",  // hyphen
		"\u2011", "-",  // non-breaking hyphen
		"\u2012", "-",  // figure dash
		"\u2013", "-",  // en dash
		"\u2014", "-",  // em dash
		"\u2015", "-",  // horizontal bar
	)
	return replacer.Replace(s)
}
