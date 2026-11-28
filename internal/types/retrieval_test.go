package types

import (
	"slices"
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
	if req.RetrievalConfig.RRFK == nil || *req.RetrievalConfig.RRFK != RetrievalRRFKDefault {
		t.Errorf("rrf_k = %v, want a pointer to the default %d", req.RetrievalConfig.RRFK, RetrievalRRFKDefault)
	}
	if req.RetrievalConfig.RerankCandidates == nil ||
		*req.RetrievalConfig.RerankCandidates != RetrievalRerankCandidatesDefault {
		t.Errorf("rerank_candidates = %v, want a pointer to the default %d",
			req.RetrievalConfig.RerankCandidates, RetrievalRerankCandidatesDefault)
	}
}

func TestRerankIsAKnownRetrievalTechnique(t *testing.T) {
	if !RetrievalTechniqueRerank.Known() {
		t.Errorf("%q is not known, want rerank registered as a technique", RetrievalTechniqueRerank)
	}
}

func TestParentChildIsAKnownRetrievalTechnique(t *testing.T) {
	if !RetrievalTechniqueParentChild.Known() {
		t.Errorf("%q is not known, want parent_child registered as a technique", RetrievalTechniqueParentChild)
	}
}

func TestQueryRewriteIsAKnownStageTechnique(t *testing.T) {
	if !RetrievalTechniqueQueryRewrite.Known() {
		t.Errorf("%q is not known, want query_rewrite registered as a technique", RetrievalTechniqueQueryRewrite)
	}
	if !RetrievalTechniqueQueryRewrite.IsStage() {
		t.Errorf("%q IsStage() = false, want it treated as a stage", RetrievalTechniqueQueryRewrite)
	}
}

func TestQueryDecompositionIsAKnownStageTechnique(t *testing.T) {
	if !RetrievalTechniqueQueryDecomposition.Known() {
		t.Errorf("%q is not known, want query_decomposition registered as a technique", RetrievalTechniqueQueryDecomposition)
	}
	if !RetrievalTechniqueQueryDecomposition.IsStage() {
		t.Errorf("%q IsStage() = false, want it treated as a stage", RetrievalTechniqueQueryDecomposition)
	}
}

func TestOnlyTheStageTechniquesAreStages(t *testing.T) {
	for technique, want := range map[RetrievalTechnique]bool{
		RetrievalTechniqueDense:              false,
		RetrievalTechniqueBM25:               false,
		RetrievalTechniqueQueryRewrite:       true,
		RetrievalTechniqueQueryDecomposition: true,
		RetrievalTechniqueRerank:             true,
		RetrievalTechniqueParentChild:        true,
	} {
		if got := technique.IsStage(); got != want {
			t.Errorf("%q IsStage() = %t, want %t", technique, got, want)
		}
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

func TestRetrievalConfigValidateAcceptsBM25Alone(t *testing.T) {
	config := RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueBM25}}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate(%+v) = %v, want bm25 accepted on its own", config, err)
	}
}

func TestRetrievalConfigValidateAcceptsSeveralTechniques(t *testing.T) {
	config := RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueBM25, RetrievalTechniqueDense}}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate(%+v) = %v, want bm25 and dense accepted together", config, err)
	}
	if config.RRFK == nil || *config.RRFK != RetrievalRRFKDefault {
		t.Errorf("rrf_k = %v, want a pointer to the default %d", config.RRFK, RetrievalRRFKDefault)
	}
}

func TestRetrievalConfigValidateChecksRRFK(t *testing.T) {
	for _, k := range []int{0, -1, RetrievalRRFKMax + 1} {
		config := RetrievalConfig{
			Techniques: []RetrievalTechnique{RetrievalTechniqueBM25},
			RRFK:       intPointer(k),
		}
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "rrf_k") {
			t.Errorf("rrf_k %d: Validate = %v, want an error naming rrf_k", k, err)
		}
	}
	for _, k := range []int{1, RetrievalRRFKMax} {
		config := RetrievalConfig{
			Techniques: []RetrievalTechnique{RetrievalTechniqueBM25},
			RRFK:       intPointer(k),
		}
		if err := config.Validate(); err != nil {
			t.Errorf("rrf_k %d: Validate = %v, want it accepted", k, err)
		}
	}
}

func TestRetrievalConfigValidateAcceptsRerankNextToACandidateTechnique(t *testing.T) {
	for _, techniques := range [][]RetrievalTechnique{
		{RetrievalTechniqueRerank, RetrievalTechniqueBM25},
		{RetrievalTechniqueDense, RetrievalTechniqueRerank},
	} {
		config := RetrievalConfig{Techniques: techniques}
		if err := config.Validate(); err != nil {
			t.Errorf("Validate(%v) = %v, want rerank accepted as a second stage", techniques, err)
		}
		if config.RerankCandidates == nil || *config.RerankCandidates != RetrievalRerankCandidatesDefault {
			t.Errorf("%v: rerank_candidates = %v, want a pointer to the default %d",
				techniques, config.RerankCandidates, RetrievalRerankCandidatesDefault)
		}
	}
}

func TestRetrievalConfigValidateRejectsRerankAlone(t *testing.T) {
	config := RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueRerank}}
	err := config.Validate()
	if err == nil {
		t.Fatalf("Validate(%v) = nil, want rerank alone rejected", config.Techniques)
	}
	for _, want := range []string{"rerank", "at least one", `"dense"`, `"bm25"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	// The candidate list must not offer rerank itself as a candidate.
	if count := strings.Count(err.Error(), `"rerank"`); count != 1 {
		t.Errorf("error %q names rerank %d times, want once plus a candidate list without it", err, count)
	}
}

func TestRetrievalConfigValidateAcceptsParentChildNextToACandidateTechnique(t *testing.T) {
	for _, techniques := range [][]RetrievalTechnique{
		{RetrievalTechniqueParentChild, RetrievalTechniqueBM25},
		{RetrievalTechniqueDense, RetrievalTechniqueParentChild},
		{RetrievalTechniqueBM25, RetrievalTechniqueRerank, RetrievalTechniqueParentChild},
	} {
		config := RetrievalConfig{Techniques: techniques}
		if err := config.Validate(); err != nil {
			t.Errorf("Validate(%v) = %v, want parent_child accepted as a final stage", techniques, err)
		}
	}
}

func TestRetrievalConfigValidateRejectsOnlyStageTechniques(t *testing.T) {
	for _, techniques := range [][]RetrievalTechnique{
		{RetrievalTechniqueParentChild},
		{RetrievalTechniqueRerank, RetrievalTechniqueParentChild},
		{RetrievalTechniqueParentChild, RetrievalTechniqueRerank},
		{RetrievalTechniqueQueryDecomposition, RetrievalTechniqueRerank},
	} {
		config := RetrievalConfig{Techniques: techniques}
		err := config.Validate()
		if err == nil {
			t.Fatalf("Validate(%v) = nil, want a stage-only config rejected", techniques)
		}
		for _, want := range []string{"at least one", `"dense"`, `"bm25"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Validate(%v) error %q does not name %q", techniques, err, want)
			}
		}
		if strings.Contains(err.Error(), `"parent_child", "dense"`) {
			t.Errorf("Validate(%v) error %q offers parent_child as a candidate", techniques, err)
		}
		if strings.Contains(err.Error(), `"query_decomposition", "dense"`) {
			t.Errorf("Validate(%v) error %q offers query_decomposition as a candidate", techniques, err)
		}
	}
}

func TestRetrievalConfigValidateRejectsQueryRewriteAlone(t *testing.T) {
	config := RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueQueryRewrite}}
	err := config.Validate()
	if err == nil {
		t.Fatalf("Validate(%v) = nil, want query_rewrite alone rejected", config.Techniques)
	}
	for _, want := range []string{"query_rewrite", "rewrites the query another technique searches with", "at least one", `"dense"`, `"bm25"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestRetrievalConfigValidateAcceptsQueryRewriteNextToACandidateTechnique(t *testing.T) {
	for _, techniques := range [][]RetrievalTechnique{
		{RetrievalTechniqueQueryRewrite, RetrievalTechniqueBM25},
		{RetrievalTechniqueDense, RetrievalTechniqueQueryRewrite},
		{RetrievalTechniqueQueryRewrite, RetrievalTechniqueBM25, RetrievalTechniqueRerank},
	} {
		config := RetrievalConfig{Techniques: techniques}
		if err := config.Validate(); err != nil {
			t.Errorf("Validate(%v) = %v, want query_rewrite accepted as a stage", techniques, err)
		}
	}
}

func TestRetrievalConfigValidateRejectsQueryDecompositionAlone(t *testing.T) {
	config := RetrievalConfig{Techniques: []RetrievalTechnique{RetrievalTechniqueQueryDecomposition}}
	err := config.Validate()
	if err == nil {
		t.Fatalf("Validate(%v) = nil, want query_decomposition alone rejected", config.Techniques)
	}
	for _, want := range []string{
		"query_decomposition",
		"splits the question into sub-questions another technique searches for",
		"at least one",
		`"dense"`,
		`"bm25"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestRetrievalConfigValidateAcceptsQueryDecompositionNextToACandidateTechnique(t *testing.T) {
	for _, techniques := range [][]RetrievalTechnique{
		{RetrievalTechniqueQueryDecomposition, RetrievalTechniqueBM25},
		{RetrievalTechniqueDense, RetrievalTechniqueQueryDecomposition},
		{RetrievalTechniqueQueryRewrite, RetrievalTechniqueQueryDecomposition, RetrievalTechniqueBM25, RetrievalTechniqueRerank},
	} {
		config := RetrievalConfig{Techniques: techniques}
		if err := config.Validate(); err != nil {
			t.Errorf("Validate(%v) = %v, want query_decomposition accepted as a stage", techniques, err)
		}
	}
}

func TestRetrievalConfigDecomposesQuery(t *testing.T) {
	for _, tc := range []struct {
		techniques []RetrievalTechnique
		want       bool
	}{
		{[]RetrievalTechnique{RetrievalTechniqueBM25}, false},
		{[]RetrievalTechnique{RetrievalTechniqueBM25, RetrievalTechniqueQueryDecomposition}, true},
	} {
		config := RetrievalConfig{Techniques: tc.techniques}
		if got := config.DecomposesQuery(); got != tc.want {
			t.Errorf("DecomposesQuery() with %v = %t, want %t", tc.techniques, got, tc.want)
		}
	}
}

func TestCandidateTechniquesLeavesOutStageTechniques(t *testing.T) {
	config := RetrievalConfig{Techniques: []RetrievalTechnique{
		RetrievalTechniqueDense,
		RetrievalTechniqueRerank,
		RetrievalTechniqueParentChild,
		RetrievalTechniqueQueryRewrite,
		RetrievalTechniqueQueryDecomposition,
		RetrievalTechniqueBM25,
	}}
	got := config.CandidateTechniques()
	want := []RetrievalTechnique{RetrievalTechniqueDense, RetrievalTechniqueBM25}
	if !slices.Equal(got, want) {
		t.Errorf("CandidateTechniques() = %v, want the candidate sources alone %v", got, want)
	}
}

func TestRetrievalConfigExpandsParents(t *testing.T) {
	for _, tc := range []struct {
		techniques []RetrievalTechnique
		want       bool
	}{
		{[]RetrievalTechnique{RetrievalTechniqueBM25, RetrievalTechniqueRerank}, false},
		{[]RetrievalTechnique{RetrievalTechniqueBM25, RetrievalTechniqueParentChild}, true},
	} {
		config := RetrievalConfig{Techniques: tc.techniques}
		if got := config.ExpandsParents(); got != tc.want {
			t.Errorf("ExpandsParents() with %v = %t, want %t", tc.techniques, got, tc.want)
		}
	}
}

func TestRetrievalConfigValidateChecksRerankCandidates(t *testing.T) {
	for _, candidates := range []int{0, -1, RetrievalRerankCandidatesMax + 1} {
		config := RetrievalConfig{
			Techniques:       []RetrievalTechnique{RetrievalTechniqueBM25, RetrievalTechniqueRerank},
			RerankCandidates: intPointer(candidates),
		}
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "rerank_candidates") {
			t.Errorf("rerank_candidates %d: Validate = %v, want an error naming rerank_candidates", candidates, err)
		}
	}
	for _, candidates := range []int{1, RetrievalRerankCandidatesMax} {
		config := RetrievalConfig{
			Techniques:       []RetrievalTechnique{RetrievalTechniqueBM25, RetrievalTechniqueRerank},
			RerankCandidates: intPointer(candidates),
		}
		if err := config.Validate(); err != nil {
			t.Errorf("rerank_candidates %d: Validate = %v, want it accepted", candidates, err)
		}
	}
}

func TestRetrievalRequestValidateRejectsARerankPoolSmallerThanTopK(t *testing.T) {
	req := RetrievalRequest{
		Query: "who won?",
		TopK:  intPointer(10),
		RetrievalConfig: &RetrievalConfig{
			Techniques:       []RetrievalTechnique{RetrievalTechniqueBM25, RetrievalTechniqueRerank},
			RerankCandidates: intPointer(5),
		},
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("Validate = nil, want a pool smaller than top_k rejected")
	}
	for _, want := range []string{"rerank_candidates", "top_k"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// A pool exactly top_k large can fill the answer.
	req.RetrievalConfig.RerankCandidates = intPointer(10)
	if err := req.Validate(); err != nil {
		t.Errorf("rerank_candidates = top_k: Validate = %v, want it accepted", err)
	}
}

func TestRetrievalRequestValidateIgnoresRerankCandidatesWhenRerankIsOff(t *testing.T) {
	req := RetrievalRequest{
		Query: "who won?",
		TopK:  intPointer(10),
		RetrievalConfig: &RetrievalConfig{
			Techniques:       []RetrievalTechnique{RetrievalTechniqueBM25},
			RerankCandidates: intPointer(5),
		},
	}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate = %v, want a valid rerank_candidates ignored without rerank", err)
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
