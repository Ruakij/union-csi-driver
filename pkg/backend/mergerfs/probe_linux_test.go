//go:build linux

package mergerfs

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestProberReportsAHungPathWithoutWaitingAgain(t *testing.T) {
	block := make(chan error)
	var calls atomic.Int32
	p := &prober{
		timeout: 10 * time.Millisecond,
		call: func(string) error {
			calls.Add(1)
			return <-block
		},
		hung: map[string]bool{},
	}

	start := time.Now()
	if err := p.statfs("/branch"); !errors.Is(err, errProbeHung) {
		t.Fatalf("statfs() = %v, want %v", err, errProbeHung)
	}
	// The second probe must not wait for the timeout again, nor start a second call.
	if err := p.statfs("/branch"); !errors.Is(err, errProbeHung) {
		t.Fatalf("second statfs() = %v, want %v", err, errProbeHung)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("two probes took %v, want about one timeout", took)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("%d calls, want 1: the hung path was probed again", got)
	}

	// Once the stuck call returns, the path is probed again.
	close(block)
	deadline := time.Now().Add(10 * time.Second)
	for p.statfs("/branch") != nil {
		if time.Now().After(deadline) {
			t.Fatal("the path stayed hung after its call returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
