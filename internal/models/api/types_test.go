package api

import (
	"slices"
	"testing"
)

// KnownAPIs is the one list of protocols this build ships: Known and the
// config error both read it, so neither can drift from the other.
func TestKnownAPIs(t *testing.T) {
	apis := KnownAPIs()
	if want := []API{APIOpenAICompletions, APIOpenAIResponses}; !slices.Equal(apis, want) {
		t.Errorf("KnownAPIs() = %v, want %v", apis, want)
	}
	for _, a := range apis {
		if !a.Known() {
			t.Errorf("KnownAPIs() lists %q, but Known() rejects it", a)
		}
	}
	if API("grpc").Known() {
		t.Error(`KnownAPIs() accepts "grpc", want it rejected`)
	}
}
