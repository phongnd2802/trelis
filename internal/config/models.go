package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Models points each model role at its own OpenAI-compatible provider.
// Every role embeds Endpoint (base_url, model, api_key) and adds only what
// is specific to it: chat roles pick a protocol; the embedding role (#16)
// and rerank role (#18) are added here as their own fields beside these.
//
// A role whose endpoint keys are all empty is unconfigured, so the service
// starts without models until a feature needs one.
type Models struct {
	// Chat answers questions (Pipeline Answer and Agent Answer).
	Chat ChatRole `mapstructure:"chat"`
	// Extraction reads Extractions out of Chunks.
	Extraction ChatRole `mapstructure:"extraction"`
	// Judge scores answers in the eval harness; it should differ from Chat.
	Judge ChatRole `mapstructure:"judge"`
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
	// Protocol is the wire protocol: chat_completions (default). responses
	// arrives with #19.
	Protocol string `mapstructure:"protocol"`
}

var chatProtocols = []string{"chat_completions"}

// chatRoles names every chat role by its key, for defaults and validation.
func (m *Models) chatRoles() map[string]*ChatRole {
	return map[string]*ChatRole{
		"models.chat":       &m.Chat,
		"models.extraction": &m.Extraction,
		"models.judge":      &m.Judge,
	}
}

// Configured reports whether any endpoint key is set.
func (e Endpoint) Configured() bool {
	return strings.TrimSpace(e.BaseURL) != "" || strings.TrimSpace(e.Model) != "" || strings.TrimSpace(e.APIKey) != ""
}

func init() {
	for key := range (&Models{}).chatRoles() {
		defaults[key+".protocol"] = "chat_completions"
		defaults[key+".base_url"] = ""
		defaults[key+".model"] = ""
		defaults[key+".api_key"] = ""
	}
}

func (m *Models) validate() []error {
	var errs []error
	for key, role := range m.chatRoles() {
		if !slices.Contains(chatProtocols, role.Protocol) {
			errs = append(errs, fmt.Errorf("%s.protocol must be one of %s, got %q", key, strings.Join(chatProtocols, ", "), role.Protocol))
		}
		errs = append(errs, role.validate(key)...)
	}
	// Map order is random; keep messages stable.
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })
	return errs
}

func (e Endpoint) validate(key string) []error {
	if !e.Configured() {
		return nil
	}
	var errs []error
	if strings.TrimSpace(e.BaseURL) == "" {
		errs = append(errs, fmt.Errorf("%s.base_url is required when the role is configured", key))
	} else if u, err := url.Parse(e.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("%s.base_url %q must be an http(s) URL", key, e.BaseURL))
	}
	if strings.TrimSpace(e.Model) == "" {
		errs = append(errs, fmt.Errorf("%s.model is required when the role is configured", key))
	}
	return errs
}
