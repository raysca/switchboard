package core

import "time"

// candidateSession shows a failover candidate the real session except for Reenter.
type candidateSession struct {
	SessionController
	idx  int
	from func(idx int, f Frame, dir Direction, resume int, gen uint64) error
}

func (c candidateSession) Reenter(f Frame, dir Direction, resume int, gen uint64) error {
	return c.from(c.idx, f, dir, resume, gen)
}

// attachCandidates attaches each of n candidate processors that is an Attacher
// through a candidateSession, stopping at the first error.
func attachCandidates(s SessionController, index, n int, proc func(i int) Processor, from func(int, Frame, Direction, int, uint64) error) error {
	for i := 0; i < n; i++ {
		if a, ok := proc(i).(Attacher); ok {
			if err := a.Attach(candidateSession{SessionController: s, idx: i, from: from}, index); err != nil {
				return err
			}
		}
	}
	return nil
}

// failoverClass is classify(err), or "" without a classifier.
func failoverClass(classify func(error) string, err error) string {
	if classify == nil {
		return ""
	}
	return classify(err)
}

// noteFailover reports one switch to the OnFailover hook and as a debug event.
func noteFailover(s SessionController, on func(from, to, class string), event, from, to, class string, turn TurnID, gen Generation) {
	if on != nil {
		on(from, to, class)
	}
	if em, ok := s.(interface{ EmitDebug(DebugEvent) }); ok {
		em.EmitDebug(NewDebugEvent(event, time.Now(), "", turn, gen,
			map[string]any{"component": from, "provider": to, "reason": class}))
	}
}
