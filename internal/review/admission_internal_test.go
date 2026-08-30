package review

import (
	"strings"
	"testing"
)

func TestAdmissionSkipsAnOverBudgetDelta(t *testing.T) {
	over := admitDelta(150, 10, 100, 60)
	if !over.Skip || !strings.Contains(over.Reason, "150") {
		t.Fatalf("verdict = %+v, want skip naming the measured size", over)
	}
	tooManyChunks := admitDelta(10, 173, 100, 60)
	if !tooManyChunks.Skip || !strings.Contains(tooManyChunks.Reason, "173") {
		t.Fatalf("verdict = %+v, want skip naming the chunk count", tooManyChunks)
	}
	within := admitDelta(100, 60, 100, 60)
	if within.Skip {
		t.Fatalf("verdict = %+v, want admission at the boundary", within)
	}
}

// GitHub's compare endpoint names at most 300 files and says nothing when a
// range holds more, so a budget above that cap would admit a delta the service
// only partly saw and then report complete coverage over it.
func TestAdmissionNeverTrustsMoreFilesThanOneCompareCanName(t *testing.T) {
	overCap := admitDelta(compareFileCap+1, 1, 1000, 60)
	if !overCap.Skip {
		t.Fatalf("verdict = %+v, want a skip: a budget above the compare cap cannot be honoured", overCap)
	}
	atCap := admitDelta(compareFileCap, 1, 1000, 60)
	if atCap.Skip {
		t.Fatalf("verdict = %+v, want admission at exactly the cap", atCap)
	}
}
