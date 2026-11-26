package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/phongnd2802/trelis/internal/models/api"
)

// Models points each model role at its own provider. Every role embeds
// Endpoint (base_url, model, api_key) and adds only what is specific to it:
// a chat role adds its protocol, the embedding role the size of the batches
// it sends. The rerank role needs nothing beyond its endpoint: the
// Jina/Cohere schema carries no setting of its own.
//
// A role with neither base_url nor model is unconfigured, so the service
// starts without models until a feature needs one.
type Models struct {
	// Chat writes answers (Pipeline Answer and Agent Answer).
	Chat ChatRole `mapstructure:"chat"`
	// Extraction reads Extractions out of Chunks.
	Extraction ChatRole `mapstructure:"extraction"`
	// Judge is the judge model a Grader uses; it should differ from Chat.
	Judge ChatRole `mapstructure:"judge"`
	// Embedding turns Chunk text into the vectors dense retrieval searches.
	Embedding EmbeddingRole `mapstructure:"embedding"`
	// Rerank scores retrieval candidates against a query, so the ones that
	// answer it best come first.
	Rerank RerankRole `mapstructure:"rerank"`
}

// Endpoint is the provider one role calls.
type Endpoint struct {
	// BaseURL is the API root, e.g. https://api.openai.com/v1.
	BaseURL string `mapstructure:"base_url"`
	Model   string `mapstructure:"model"`
	// APIKey is sent as a bearer token; empty sends none.
	APIKey string `mapstructure:"api_key"`
}

// ChatRole is a role served by a chat model.
type ChatRole struct {
	Endpoint `mapstructure:",squash"`
	// Protocol is the wire protocol, in the model layer's vocabulary:
	// openai-completions (default) or openai-responses.
	Protocol api.API `mapstructure:"protocol"`
}

// embeddingKey names the embedding role where its key is spelled out: in the
// defaults, in the env variable that overrides them and in errors.
const embeddingKey = "models.embedding"

// DefaultEmbeddingBatchSize is how many texts one embeddings request carries
// when models.embedding.batch_size leaves it out. The client falls back to it
// as well, so a role built in code batches the way a loaded one does.
const DefaultEmbeddingBatchSize = 64

// EmbeddingRole is a role served by an embedding model over /v1/embeddings.
type EmbeddingRole struct {
	Endpoint `mapstructure:",squash"`
	// BatchSize is how many texts one request carries. A provider answers a
	// batch whole or rejects it, so this has to stay under the model's own
	// input limit: a smaller batch costs more requests but loses less work
	// when one of them fails.
	BatchSize int `mapstructure:"batch_size"`
}

// rerankKey names the rerank role where its key is spelled out: in the
// defaults, in the env variable that overrides them and in errors.
const rerankKey = "models.rerank"

// RerankRole is a role served by a rerank model over /v1/rerank. The
// Jina/Cohere schema the endpoint speaks has no setting beyond the endpoint
// itself: the client always asks for a score per document, in the caller's
// order.
type RerankRole struct {
	Endpoint `mapstructure:",squash"`
}

// chatRoles names every chat role by its key, for defaults and validation.
func (m *Models) chatRoles() map[string]*ChatRole {
	return map[string]*ChatRole{
		"models.chat":       &m.Chat,
		"models.extraction": &m.Extraction,
		"models.judge":      &m.Judge,
	}
}

func init() {
	for key := range (&Models{}).chatRoles() {
		registerEndpoint(key)
		defaults[key+".protocol"] = string(api.APIOpenAICompletions)
	}
	registerEndpoint(embeddingKey)
	defaults[embeddingKey+".batch_size"] = DefaultEmbeddingBatchSize
	registerEndpoint(rerankKey)
}

// registerEndpoint adds the endpoint keys under key to the defaults, so env
// variables can set a role the YAML file leaves out.
func registerEndpoint(key string) {
	for _, k := range []string{"base_url", "model", "api_key"} {
		defaults[key+"."+k] = ""
	}
}

// Configured reports whether the role names a provider. An api_key alone
// does not count, so a key left in the environment cannot block startup.
func (e Endpoint) Configured() bool {
	return strings.TrimSpace(e.BaseURL) != "" || strings.TrimSpace(e.Model) != ""
}

// APIEndpoint is the endpoint a role's client calls: the same base URL and
// model, with the key turned into the auth the wire wants. role names the
// caller in an error ("chat", "embedding", "rerank"), so a message says
// which role is missing its base URL or its model.
func (e Endpoint) APIEndpoint(role string) (api.Endpoint, error) {
	if strings.TrimSpace(e.BaseURL) == "" {
		return api.Endpoint{}, fmt.Errorf("%s: base URL is required", role)
	}
	if strings.TrimSpace(e.Model) == "" {
		return api.Endpoint{}, fmt.Errorf("%s: model is required", role)
	}
	return api.Endpoint{BaseURL: e.BaseURL, Model: e.Model, Auth: api.BearerAuth(e.APIKey)}, nil
}

func (m *Models) validate() []error {
	var errs []error
	// The list comes from the model layer, so the protocols Known accepts and
	// the ones this message names cannot drift apart.
	protocols := make([]string, 0, len(api.KnownAPIs()))
	for _, known := range api.KnownAPIs() {
		protocols = append(protocols, string(known))
	}
	for key, role := range m.chatRoles() {
		if !role.Protocol.Known() {
			errs = append(errs, fmt.Errorf("%s.protocol %q is not a protocol this build ships (want %s)",
				key, role.Protocol, strings.Join(protocols, " or ")))
		}
		errs = append(errs, role.validate(key)...)
	}
	errs = append(errs, m.Embedding.validate(embeddingKey)...)
	errs = append(errs, m.Rerank.validate(rerankKey)...)
	// Map order is random; keep messages stable.
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}

// validate checks a configured endpoint under key.
func (e Endpoint) validate(key string) []error {
	if !e.Configured() {
		return nil
	}
	var errs []error
	if strings.TrimSpace(e.BaseURL) == "" {
		errs = append(errs, fmt.Errorf("%s.base_url is required when %s.model is set", key, key))
	} else if u, err := url.Parse(e.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("%s.base_url %q must be an http(s) URL", key, e.BaseURL))
	}
	if strings.TrimSpace(e.Model) == "" {
		errs = append(errs, fmt.Errorf("%s.model is required when %s.base_url is set", key, key))
	}
	return errs
}

// validate checks the embedding role under key: its endpoint like every other
// role, plus the batch size. A batch size only matters once a provider is
// named, so an unconfigured role is left as it is.
func (r EmbeddingRole) validate(key string) []error {
	errs := r.Endpoint.validate(key)
	if r.Configured() && r.BatchSize < 1 {
		errs = append(errs, fmt.Errorf("%s.batch_size must be at least 1, got %d", key, r.BatchSize))
	}
	return errs
}
