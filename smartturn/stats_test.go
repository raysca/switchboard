package smartturn

// Fallbacks and Crashes read the counters that WriteMetrics exports; only tests need them.
func Fallbacks(reason string) uint64 {
	for i, r := range reasons {
		if r == reason {
			return stats.fallbacks[i].Load()
		}
	}
	return 0
}

func Crashes() uint64 { return stats.crashes.Load() }
