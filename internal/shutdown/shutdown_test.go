package shutdown

import (
	"context"
	"testing"
)

func TestSignalIsTheChannelItCarried(t *testing.T) {
	done := make(chan struct{})
	if got := Signal(WithSignal(context.Background(), done)); got != done {
		t.Errorf("Signal = %v, want the channel WithSignal carried", got)
	}
	if got := Signal(context.Background()); got != nil {
		t.Errorf("Signal of a plain context = %v, want nil", got)
	}
}
