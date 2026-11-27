package types

import (
	"strings"
	"testing"
)

// intPointer returns a pointer to n, for an explicitly given top_k.
func intPointer(n int) *int { return &n }

func TestRetrievalRequestValidateFillsTheDefaults(t *testing.T) {
	req := RetrievalRequest{Query: "  who won?  "}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if req.Query != "who won?" {
		t.Errorf("query = %q, want the trimmed text", req.Query)
	}
	if req.TopK == nil || *req.TopK != RetrievalTopKDefault {
		t.Errorf("top_k = %v, want a pointer to the default %d", req.TopK, RetrievalTopKDefault)
	}
	if req.RetrievalConfig == nil || len(req.RetrievalConfig.Techniques) != 1 ||
		req.RetrievalConfig.Techniques[0] != RetrievalTechniqueDense {
		t.Errorf("retrieval_config = %+v, want dense alone", req.RetrievalConfig)
	}
}

func TestRetrievalRequestValidateKeepsAConfiguredTechnique(t *testing.T) {
	req := RetrievalRequest{
		Query:           "who won?",
		TopK:            intPointer(5),
		RetrievalConfig: &RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueDense}},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if req.TopK == nil || *req.TopK != 5 {
		t.Errorf("top_k = %v, want a pointer to 5", req.TopK)
	}
}

func TestRetrievalConfigValidateRejectsBadTechniqueLists(t *testing.T) {
	for name, tc := range map[string]struct {
		config  RetrievalConfig
		wantMsg []string
	}{
		"empty":     {RetrievalConfig{}, []string{"at least one", "dense"}},
		"duplicate": {RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueDense, RetrievalTechniqueDense}}, []string{"dense", "more than once"}},
		"bm25":      {RetrievalConfig{Techniques: []RetrievalTechnique{"bm25"}}, []string{"bm25", "dense"}},
		"unknown":   {RetrievalConfig{Techniques: []RetrievalTechnique{"foo"}}, []string{"foo", "dense"}},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.config.Validate()
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.config)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestRetrievalRequestValidateChecksTopK(t *testing.T) {
	for _, topK := range []int{-1, 0, 101} {
		req := RetrievalRequest{Query: "who won?", TopK: intPointer(topK)}
		if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "top_k") {
			t.Errorf("top_k %d: Validate = %v, want an error naming top_k", topK, err)
		}
	}
	for _, topK := range []int{1, 100} {
		req := RetrievalRequest{Query: "who won?", TopK: intPointer(topK)}
		if err := req.Validate(); err != nil {
			t.Errorf("top_k %d: Validate = %v, want it accepted", topK, err)
		}
	}
}

func TestRetrievalRequestValidateRejectsABlankQuery(t *testing.T) {
	for _, query := range []string{"", "   ", "\n\t"} {
		req := RetrievalRequest{Query: query}
		if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "query") {
			t.Errorf("query %q: Validate = %v, want an error naming query", query, err)
		}
	}
}

func TestRetrievalRequestValidateCapsTheQueryLength(t *testing.T) {
	req := RetrievalRequest{Query: strings.Repeat("a", RetrievalQueryMaxRunes)}
	if err := req.Validate(); err != nil {
		t.Errorf("%d-rune query: Validate = %v, want it accepted", RetrievalQueryMaxRunes, err)
	}
	req = RetrievalRequest{Query: strings.Repeat("a", RetrievalQueryMaxRunes+1)}
	if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "2000") {
		t.Errorf("%d-rune query: Validate = %v, want an error naming the limit", RetrievalQueryMaxRunes+1, err)
	}
}
