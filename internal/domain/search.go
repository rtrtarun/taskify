package domain

import (
	"slices"
	"strings"
	"unicode"
)

// maxSearchTerms bounds the words a similarity search uses.
const maxSearchTerms = 12

// searchStopwords are common words that say nothing about what a task is.
var searchStopwords = []string{"the", "and", "for", "with", "from", "into", "when", "this", "that", "are", "was",
	"not", "but", "has", "have", "can", "cannot", "after", "before", "all", "any", "our", "out", "its"}

// SearchTerms returns the distinct lowercase words of text worth matching:
// letters and digits only, at least three long, common words dropped, at
// most twelve. Because each term is plain letters and digits, terms can be
// joined into a full-text query without any escaping.
func SearchTerms(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var out []string
	for _, w := range words {
		if len([]rune(w)) < 3 || slices.Contains(searchStopwords, w) || slices.Contains(out, w) {
			continue
		}
		out = append(out, w)
		if len(out) == maxSearchTerms {
			break
		}
	}
	return out
}
