package core

import "context"

// Small helpers for the in-package tests. The external tests (package core_test) keep their own
// copies, and use coretest for the larger doubles: coretest imports core, so an in-package test
// cannot import it.

type testFnProcessor struct {
	fn func(ctx context.Context, f Frame, dir Direction, e Emitter) error
}

func (p *testFnProcessor) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	return p.fn(ctx, f, dir, e)
}
