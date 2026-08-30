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
