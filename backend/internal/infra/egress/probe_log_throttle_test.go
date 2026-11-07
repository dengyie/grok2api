package egress

import (
	"sync"
	"testing"
)

func TestProbeLogThrottleReportsOnlyFreshFailures(t *testing.T) {
	var throttle probeLogThrottle

	if !throttle.shouldLogFailure(7, "ipv6") {
		t.Fatal("first failure for a node must be reported")
	}
	for attempt := range 5 {
		if throttle.shouldLogFailure(7, "ipv6") {
			t.Fatalf("repeated failure %d must not be reported", attempt)
		}
	}
	throttle.observeSuccess(7, "ipv6")
	if !throttle.shouldLogFailure(7, "ipv6") {
		t.Fatal("a failure after a recovery must be reported again")
	}
}

func TestProbeLogThrottleKeepsFamiliesIndependent(t *testing.T) {
	var throttle probeLogThrottle

	// IPv4 is healthy while IPv6 is down. A family-agnostic throttle would flip
	// its state twice per cycle here and re-report the same IPv6 failure forever.
	throttle.observeSuccess(7, "ipv4")
	if !throttle.shouldLogFailure(7, "ipv6") {
		t.Fatal("ipv6 entering failure must be reported")
	}
	throttle.observeSuccess(7, "ipv4")
	if throttle.shouldLogFailure(7, "ipv6") {
		t.Fatal("ipv6 staying failed must not be reported while ipv4 succeeds")
	}
}

func TestProbeLogThrottleTracksNodesSeparately(t *testing.T) {
	var throttle probeLogThrottle

	if !throttle.shouldLogFailure(7, "ipv4") {
		t.Fatal("node 7 entering failure must be reported")
	}
	if !throttle.shouldLogFailure(8, "ipv4") {
		t.Fatal("node 8 entering failure must be reported independently")
	}
	if throttle.shouldLogFailure(7, "ipv4") {
		t.Fatal("node 7 repeat must stay suppressed after node 8 changed")
	}
}

func TestProbeLogThrottleConcurrentUse(t *testing.T) {
	var throttle probeLogThrottle
	var wg sync.WaitGroup
	for index := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = throttle.shouldLogFailure(uint64(index), "ipv4")
		}()
	}
	wg.Wait()

	if throttle.shouldLogFailure(0, "ipv4") {
		t.Fatal("repeat after concurrent use must stay suppressed")
	}
}
