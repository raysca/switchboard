package core

// SplitNext exposes the production segmenter (nextSplit) to offline tools such as tools/ttssim, so
// simulated strategies use the real sentence rules. Same contract as nextSplit: the byte index at
// which buf's next unit ends, or -1 to wait. first/last mirror splitRules.
func SplitNext(buf string, min, max, clause int, first, last bool) int {
	return nextSplit(buf, splitRules{min: min, max: max, clause: clause, first: first, last: last})
}
