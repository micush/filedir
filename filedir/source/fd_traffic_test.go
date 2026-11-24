package main

import (
	"testing"
	"time"
)

func TestTrafficSamplesRatesAndKeepsAHalfHour(t *testing.T) {
	var tr fdTraffic
	t0 := time.Unix(1_700_000_000, 0)
	tr.sample(t0) // the first sample only sets the counters
	if len(tr.history()) != 0 {
		t.Fatal("a sample without an interval")
	}
	tr.rx.Add(50_000_000)
	tr.tx.Add(10_000_000)
	tr.sample(t0.Add(5 * time.Second))
	h := tr.history()
	if len(h) != 1 || h[0].Rx != 10_000_000 || h[0].Tx != 2_000_000 || h[0].T != t0.Add(5*time.Second).Unix() {
		t.Fatalf("history: %+v", h)
	}
	tr.sample(t0.Add(10 * time.Second)) // nothing moved: a zero, not a repeat
	if h := tr.history(); len(h) != 2 || h[1].Rx != 0 || h[1].Tx != 0 {
		t.Fatalf("an idle interval: %+v", h)
	}
	for i := 0; i < fdTrafficKeep+50; i++ {
		tr.sample(t0.Add(time.Duration(15+5*i) * time.Second))
	}
	h = tr.history()
	if len(h) != fdTrafficKeep {
		t.Fatalf("kept %d samples, want %d", len(h), fdTrafficKeep)
	}
	if h[len(h)-1].T <= h[0].T {
		t.Fatal("the history is not oldest first")
	}
}
