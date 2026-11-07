package egress

import "sync"

// probeLogKey identifies one probe stream for logging purposes. A node is
// probed once per address family, so keying on the family as well keeps a node
// whose IPv4 recovers but whose IPv6 stays down from flipping state twice per
// cycle and re-logging the same failure forever.
type probeLogKey struct {
	nodeID uint64
	family string
}

// probeLogThrottle decides whether a probe failure deserves a log line. An
// egress host that stays down is a steady state, not an event: announcing it
// every cycle buries the real transitions (a second node dying, a node
// recovering) under one dead node's repeats.
//
// A failure is announced only when it is new for that node and family. Success
// is not announced through this type because the healthy path already logs at
// info level on its own.
type probeLogThrottle struct {
	mu      sync.Mutex
	failing map[probeLogKey]bool
}

// shouldLogFailure records a failure and reports whether it is a fresh one.
// An unseen key counts as fresh so a failure already present at startup is
// still reported once.
func (t *probeLogThrottle) shouldLogFailure(nodeID uint64, family string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := probeLogKey{nodeID: nodeID, family: family}
	if t.failing == nil {
		t.failing = make(map[probeLogKey]bool)
	}
	if t.failing[key] {
		return false
	}
	t.failing[key] = true
	return true
}

// observeSuccess clears the failure state so the next failure for this node
// and family is announced again.
func (t *probeLogThrottle) observeSuccess(nodeID uint64, family string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.failing, probeLogKey{nodeID: nodeID, family: family})
}
