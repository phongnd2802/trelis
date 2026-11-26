package fakeprovider_test

import (
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/fakeprovider"
)

type embeddingList struct {
	Object string `json:"object"`
	Model  string `json:"model"`
	Data   []struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func embed(t *testing.T, p *fakeprovider.Provider, body map[string]any) embeddingList {
	t.Helper()
	status, raw := post(t, p, "/embeddings", body)
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, raw)
	}
	return decode[embeddingList](t, raw)
}

func TestEmbeddingsAreDeterministicUnitVectorsPerInput(t *testing.T) {
	p := fakeprovider.New(t)
	got := embed(t, p, map[string]any{"model": "emb", "input": []string{"Luật Đất đai", "Nghị định 43", "Luật Đất đai"}})
	if got.Object != "list" || got.Model != "emb" || len(got.Data) != 3 {
		t.Fatalf("response = %+v", got)
	}
	for i, d := range got.Data {
		if d.Object != "embedding" || d.Index != i || len(d.Embedding) != fakeprovider.DefaultEmbeddingDims {
			t.Errorf("data[%d] = %+v", i, d)
		}
		var norm float64
		for _, x := range d.Embedding {
			norm += x * x
		}
		if math.Abs(norm-1) > 1e-6 {
			t.Errorf("data[%d] has norm² %v, want 1", i, norm)
		}
	}
	if !slices.Equal(got.Data[0].Embedding, got.Data[2].Embedding) {
		t.Error("the same text got different vectors")
	}
	if slices.Equal(got.Data[0].Embedding, got.Data[1].Embedding) {
		t.Error("different texts got the same vector")
	}
	if got.Usage.PromptTokens == 0 || got.Usage.TotalTokens != got.Usage.PromptTokens {
		t.Errorf("usage = %+v", got.Usage)
	}

	// A second provider gives the same vector for the same text.
	single := embed(t, fakeprovider.New(t), map[string]any{"model": "emb", "input": "Luật Đất đai"})
	if !slices.Equal(single.Data[0].Embedding, got.Data[0].Embedding) {
		t.Error("vectors differ across providers")
	}
}

func TestEmbeddingDimsComeFromTheRequestOrTheProvider(t *testing.T) {
	p := fakeprovider.New(t)
	if got := embed(t, p, map[string]any{"model": "emb", "input": "x", "dimensions": 3}); len(got.Data[0].Embedding) != 3 {
		t.Errorf("with dimensions=3: len = %d", len(got.Data[0].Embedding))
	}
	p.SetEmbeddingDims(1024)
	if got := embed(t, p, map[string]any{"model": "emb", "input": "x"}); len(got.Data[0].Embedding) != 1024 {
		t.Errorf("after SetEmbeddingDims(1024): len = %d", len(got.Data[0].Embedding))
	}
}

func TestEmbedAsPinsAVector(t *testing.T) {
	p := fakeprovider.New(t)
	p.EmbedAs("tax", []float32{1, 0, 0})
	got := embed(t, p, map[string]any{"model": "emb", "input": "tax"})
	if !slices.Equal(got.Data[0].Embedding, []float64{1, 0, 0}) {
		t.Errorf("embedding = %v", got.Data[0].Embedding)
	}
}

type rerankResponse struct {
	Model   string `json:"model"`
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
		Document       *struct {
			Text string `json:"text"`
		} `json:"document"`
	} `json:"results"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

func TestRerankScoresByWordOverlapBestFirst(t *testing.T) {
	p := fakeprovider.New(t)
	status, raw := post(t, p, "/rerank", map[string]any{
		"model": "rr",
		"query": "land use tax",
		"documents": []any{
			"weather report",
			map[string]any{"text": "land use tax rates"},
			"land registry",
		},
		"return_documents": true,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, raw)
	}
	got := decode[rerankResponse](t, raw)
	var order []int
	for _, r := range got.Results {
		order = append(order, r.Index)
	}
	if !slices.Equal(order, []int{1, 2, 0}) {
		t.Fatalf("result order = %v, want [1 2 0]", order)
	}
	if got.Results[0].RelevanceScore != 1 || got.Results[2].RelevanceScore != 0 {
		t.Errorf("scores = %+v, want 1 for full overlap and 0 for none", got.Results)
	}
	if got.Results[0].Document == nil || got.Results[0].Document.Text != "land use tax rates" {
		t.Errorf("document = %+v", got.Results[0].Document)
	}
	if got.Model != "rr" || got.Usage.TotalTokens == 0 {
		t.Errorf("model, usage = %q, %+v", got.Model, got.Usage)
	}
}

func TestRerankHonoursTopNAndPinnedScores(t *testing.T) {
	p := fakeprovider.New(t)
	p.ScoreAs("tax", "weather report", 0.9)
	_, raw := post(t, p, "/rerank", map[string]any{
		"model": "rr", "query": "tax", "documents": []string{"tax law", "weather report", "other"}, "top_n": 2,
	})
	got := decode[rerankResponse](t, raw)
	if len(got.Results) != 2 || got.Results[0].Index != 0 || got.Results[1].Index != 1 || got.Results[1].RelevanceScore != 0.9 {
		t.Errorf("results = %+v", got.Results)
	}
	if got.Results[0].Document != nil {
		t.Errorf("document returned without return_documents")
	}
}

func TestFailNextQueuesErrorsBeforeAnswering(t *testing.T) {
	p := fakeprovider.New(t)
	p.FailNext(fakeprovider.Embeddings, http.StatusTooManyRequests, 2)
	p.FailNext(fakeprovider.Embeddings, http.StatusBadGateway, 1)

	body := map[string]any{"model": "emb", "input": "x"}
	var statuses []int
	for range 4 {
		status, raw := post(t, p, "/embeddings", body)
		statuses = append(statuses, status)
		if status != http.StatusOK && !strings.Contains(string(raw), `"error"`) {
			t.Errorf("failure body %s is not an OpenAI error", raw)
		}
	}
	if want := []int{429, 429, 502, 200}; !slices.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
	if n := len(p.Requests(fakeprovider.Embeddings)); n != 4 {
		t.Errorf("recorded %d embeddings requests, want 4", n)
	}
	if n := len(p.Requests(fakeprovider.Rerank)); n != 0 {
		t.Errorf("recorded %d rerank requests, want 0", n)
	}
	if got := p.Requests(fakeprovider.Embeddings)[0].Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("recorded Authorization = %q", got)
	}
}
