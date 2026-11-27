package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeAnswerRequest unmarshals body into an AnswerRequest, failing the
// test when the JSON is malformed.
func decodeAnswerRequest(t *testing.T, body string) *AnswerRequest {
	t.Helper()
	var req AnswerRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return &req
}

func TestAnswerRequestFillsTheRetrievalDefaults(t *testing.T) {
	req := decodeAnswerRequest(t, `{"query":"  who won?  "}`)
	if req.Debug {
		t.Error("debug = true, want false when the body leaves it out")
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if req.Query != "who won?" {
		t.Errorf("query = %q, want the trimmed question", req.Query)
	}
	if req.TopK == nil || *req.TopK != RetrievalTopKDefault {
		t.Errorf("top_k = %v, want the default %d", req.TopK, RetrievalTopKDefault)
	}
	if req.RetrievalConfig == nil || len(req.RetrievalConfig.Techniques) != 1 ||
		req.RetrievalConfig.Techniques[0] != RetrievalTechniqueDense {
		t.Errorf("retrieval_config = %+v, want dense alone", req.RetrievalConfig)
	}
}

func TestAnswerRequestDecodesTheRetrievalFieldsFlatAndDebug(t *testing.T) {
	req := decodeAnswerRequest(t,
		`{"query":"thue tncn","top_k":3,"retrieval_config":{"techniques":["bm25"],"rrf_k":10},"debug":true}`)
	if !req.Debug {
		t.Error("debug = false, want true")
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if req.Query != "thue tncn" || req.TopK == nil || *req.TopK != 3 ||
		req.RetrievalConfig == nil || len(req.RetrievalConfig.Techniques) != 1 ||
		req.RetrievalConfig.Techniques[0] != RetrievalTechniqueBM25 ||
		req.RetrievalConfig.RRFK == nil || *req.RetrievalConfig.RRFK != 10 {
		t.Errorf("request = %+v, want the decoded fields", req)
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The retrieval fields are promoted, exactly as the body spells them.
	for _, key := range []string{`"query":"thue tncn"`, `"top_k":3`, `"retrieval_config"`, `"debug":true`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("marshalled request %s is missing %s", raw, key)
		}
	}
	if strings.Contains(string(raw), `"RetrievalRequest"`) {
		t.Errorf("marshalled request %s nests the retrieval request", raw)
	}
}

func TestAnswerRequestRejectsWhatRetrievalRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		wantMsg []string
	}{
		"blank query": {
			body:    `{"query":"   "}`,
			wantMsg: []string{"query"},
		},
		"top_k zero": {
			body:    `{"query":"who won?","top_k":0}`,
			wantMsg: []string{"top_k"},
		},
		"top_k too large": {
			body:    `{"query":"who won?","top_k":101}`,
			wantMsg: []string{"top_k"},
		},
		"unknown technique": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["foo"]}}`,
			wantMsg: []string{"foo", "dense"},
		},
		"rerank alone": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["rerank"]}}`,
			wantMsg: []string{"rerank", "at least one"},
		},
		"rrf_k out of range": {
			body:    `{"query":"who won?","retrieval_config":{"techniques":["bm25"],"rrf_k":0}}`,
			wantMsg: []string{"rrf_k", "1 and 1000"},
		},
		"query too long": {
			body:    `{"query":"` + strings.Repeat("a", RetrievalQueryMaxRunes+1) + `"}`,
			wantMsg: []string{"2000"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := decodeAnswerRequest(t, tc.body)
			err := req.Validate()
			if err == nil {
				t.Fatal("Validate = nil, want an error")
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}
