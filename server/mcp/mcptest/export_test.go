package mcptest

func (b *BlackHole) Accepted() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accepted
}
