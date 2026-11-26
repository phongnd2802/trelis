package config

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/phongnd2802/trelis/internal/models/api"
)

// Models points each model role at its own OpenAI-compatible provider.
// Every role embeds Endpoint (base_url, model, api_key) and adds only what
// is specific to it: a chat role adds its protocol. The embedding (#16) and
// rerank (#18) roles become fields here of their own types that embed
// Endpoint too, registered with registerEndpoint and checked with
// Endpoint.validate.
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
	// openai-completions (default); openai-responses joins with #19.
	Protocol api.API `mapstructure:"protocol"`
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

func (m *Models) validate() []error {
	var errs []error
	for key, role := range m.chatRoles() {
		if !role.Protocol.Known() {
			errs = append(errs, fmt.Errorf("%s.protocol %q is not a protocol this build ships (want %s)", key, role.Protocol, api.APIOpenAICompletions))
		}
		errs = append(errs, role.validate(key)...)
	}
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
