package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phongnd2802/trelis/internal/models/api"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trelis.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validYAML = `
server:
  addr: ":9090"
  shutdown_timeout: 5s
`

func TestLoadReadsYAML(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("Server.Addr = %q, want %q", cfg.Server.Addr, ":9090")
	}
	if cfg.Server.ShutdownTimeout != 5*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v, want 5s", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadEnvOverridesIndividualKeys(t *testing.T) {
	t.Setenv("TRELIS_SERVER_ADDR", "127.0.0.1:7000")

	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:7000" {
		t.Errorf("Server.Addr = %q, want the env value 127.0.0.1:7000", cfg.Server.Addr)
	}
	if cfg.Server.ShutdownTimeout != 5*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v, want 5s from YAML", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadEnvOverridesKeyMissingFromYAML(t *testing.T) {
	t.Setenv("TRELIS_SERVER_SHUTDOWN_TIMEOUT", "30s")

	cfg, err := Load(writeConfig(t, "server:\n  addr: \":8080\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 30*time.Second {
		t.Errorf("Server.ShutdownTimeout = %v, want 30s from env", cfg.Server.ShutdownTimeout)
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		env     map[string]string
		wantMsg string
	}{
		{"unknown key", validYAML + "  port: 1\n", nil, "port"},
		{"empty addr", "server:\n  addr: \"\"\n  shutdown_timeout: 5s\n", nil, "server.addr"},
		{"addr without port", "server:\n  addr: \"localhost\"\n  shutdown_timeout: 5s\n", nil, "server.addr"},
		{"zero shutdown timeout", "server:\n  addr: \":8080\"\n  shutdown_timeout: 0s\n", nil, "server.shutdown_timeout"},
		{"shutdown timeout without unit", "server:\n  addr: \":8080\"\n  shutdown_timeout: 5\n", nil, "server.shutdown_timeout"},
		{"bad env addr names the env var", validYAML, map[string]string{"TRELIS_SERVER_ADDR": "localhost"}, "TRELIS_SERVER_ADDR"},
		{"bad duration", "server:\n  addr: \":8080\"\n  shutdown_timeout: soon\n", nil, "shutdown_timeout"},
		{"bad env duration names the env var", validYAML, map[string]string{"TRELIS_SERVER_SHUTDOWN_TIMEOUT": "soon"}, "TRELIS_SERVER_SHUTDOWN_TIMEOUT"},
		{"malformed yaml", "server: [\n", nil, "trelis.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestLoadMissingFileNamesThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("Load(missing) err = %v, want it to name %s", err, path)
	}
}

const depsYAML = `
server:
  addr: ":8080"
  shutdown_timeout: 5s
postgres:
  host: db
  port: 5433
  dbname: trelis
  user: trelis
  password: from-yaml
neo4j:
  host: graph
  port: 7688
  user: neo4j
  password: graphpw
temporal:
  host: temporal
  port: 7234
  namespace: trelis
`

func TestLoadDependencies(t *testing.T) {
	t.Setenv("TRELIS_POSTGRES_PASSWORD", "s3cr/t @x")

	cfg, err := Load(writeConfig(t, depsYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Postgres.DSN(), "postgres://trelis:s3cr%2Ft%20%40x@db:5433/trelis?sslmode=disable"; got != want {
		t.Errorf("Postgres.DSN() = %q, want %q", got, want)
	}
	if got, want := cfg.Neo4j.URI(), "bolt://graph:7688"; got != want {
		t.Errorf("Neo4j.URI() = %q, want %q", got, want)
	}
	if cfg.Neo4j.User != "neo4j" || cfg.Neo4j.Password != "graphpw" {
		t.Errorf("Neo4j credentials = %q/%q, want neo4j/graphpw", cfg.Neo4j.User, cfg.Neo4j.Password)
	}
	if got, want := cfg.Temporal.HostPort(), "temporal:7234"; got != want {
		t.Errorf("Temporal.HostPort() = %q, want %q", got, want)
	}
	if cfg.Temporal.Namespace != "trelis" {
		t.Errorf("Temporal.Namespace = %q, want trelis", cfg.Temporal.Namespace)
	}
}

func TestLoadDependencyDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Postgres.DSN(), "postgres://trelis@localhost:5432/trelis?sslmode=disable"; got != want {
		t.Errorf("default Postgres.DSN() = %q, want %q", got, want)
	}
	if got, want := cfg.Neo4j.URI(), "bolt://localhost:7687"; got != want {
		t.Errorf("default Neo4j.URI() = %q, want %q", got, want)
	}
	if got, want := cfg.Temporal.HostPort(), "localhost:7233"; got != want {
		t.Errorf("default Temporal.HostPort() = %q, want %q", got, want)
	}
	if cfg.Storage.MaxFileSize() != 50<<20 {
		t.Errorf("default max file size = %d bytes, want 50 MB", cfg.Storage.MaxFileSize())
	}
	if s := cfg.Storage.S3; s.Bucket != "trelis" || s.Region != "us-east-1" || s.CreateBucket || s.Endpoint != "" {
		t.Errorf("default storage.s3 = %+v, want bucket trelis in us-east-1, not created on start, AWS endpoint", s)
	}
}

const modelsYAML = `
server:
  addr: ":8080"
  shutdown_timeout: 5s
models:
  chat:
    base_url: http://litellm:4000/v1
    model: gpt-4o-mini
    api_key: sk-chat
  extraction:
    protocol: openai-completions
    base_url: https://openrouter.ai/api/v1
    model: qwen/qwen3-32b
  judge:
    base_url: https://api.openai.com/v1
    model: gpt-4.1
  embedding:
    base_url: http://litellm:4000/v1
    model: text-embedding-3-small
    api_key: sk-embed
    batch_size: 16
  rerank:
    base_url: https://api.jina.ai/v1
    model: jina-reranker-v2-base-multilingual
    api_key: sk-rerank
`

func TestLoadModelRoles(t *testing.T) {
	t.Setenv("TRELIS_MODELS_JUDGE_API_KEY", "sk-judge")

	cfg, err := Load(writeConfig(t, modelsYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Models{
		Chat: ChatRole{Protocol: "openai-completions", Endpoint: Endpoint{
			BaseURL: "http://litellm:4000/v1", Model: "gpt-4o-mini", APIKey: "sk-chat"}},
		Extraction: ChatRole{Protocol: "openai-completions", Endpoint: Endpoint{
			BaseURL: "https://openrouter.ai/api/v1", Model: "qwen/qwen3-32b"}},
		Judge: ChatRole{Protocol: "openai-completions", Endpoint: Endpoint{
			BaseURL: "https://api.openai.com/v1", Model: "gpt-4.1", APIKey: "sk-judge"}},
		Embedding: EmbeddingRole{BatchSize: 16, Endpoint: Endpoint{
			BaseURL: "http://litellm:4000/v1", Model: "text-embedding-3-small", APIKey: "sk-embed"}},
		Rerank: RerankRole{Endpoint: Endpoint{
			BaseURL: "https://api.jina.ai/v1", Model: "jina-reranker-v2-base-multilingual", APIKey: "sk-rerank"}},
	}
	if cfg.Models != want {
		t.Errorf("Models = %+v,\nwant     %+v", cfg.Models, want)
	}
}

func TestLoadRerankRoleOnItsOwn(t *testing.T) {
	t.Setenv("TRELIS_MODELS_RERANK_BASE_URL", "http://localhost:4000/v1")
	t.Setenv("TRELIS_MODELS_RERANK_MODEL", "local-rerank")
	t.Setenv("TRELIS_MODELS_RERANK_API_KEY", "sk-local")

	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := RerankRole{Endpoint: Endpoint{
		BaseURL: "http://localhost:4000/v1", Model: "local-rerank", APIKey: "sk-local"}}
	if cfg.Models.Rerank != want {
		t.Errorf("Models.Rerank = %+v,\nwant          %+v", cfg.Models.Rerank, want)
	}
	// A provider for one role is not a provider for the others.
	if cfg.Models.Chat.Configured() || cfg.Models.Extraction.Configured() ||
		cfg.Models.Judge.Configured() || cfg.Models.Embedding.Configured() {
		t.Errorf("Models = %+v, want the other roles unconfigured", cfg.Models)
	}
}

func TestLoadEmbeddingRoleOnItsOwn(t *testing.T) {
	t.Setenv("TRELIS_MODELS_EMBEDDING_BASE_URL", "http://localhost:4000/v1")
	t.Setenv("TRELIS_MODELS_EMBEDDING_MODEL", "local-embed")
	t.Setenv("TRELIS_MODELS_EMBEDDING_API_KEY", "sk-local")

	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := EmbeddingRole{BatchSize: DefaultEmbeddingBatchSize, Endpoint: Endpoint{
		BaseURL: "http://localhost:4000/v1", Model: "local-embed", APIKey: "sk-local"}}
	if cfg.Models.Embedding != want {
		t.Errorf("Models.Embedding = %+v,\nwant             %+v", cfg.Models.Embedding, want)
	}
	// A provider for one role is not a provider for the others.
	if cfg.Models.Chat.Configured() || cfg.Models.Extraction.Configured() || cfg.Models.Judge.Configured() {
		t.Errorf("Models = %+v, want the chat roles unconfigured", cfg.Models)
	}
}

func TestLoadEmbeddingBatchSizeFromEnv(t *testing.T) {
	t.Setenv("TRELIS_MODELS_EMBEDDING_BATCH_SIZE", "8")

	cfg, err := Load(writeConfig(t, modelsYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Models.Embedding.BatchSize; got != 8 {
		t.Errorf("Models.Embedding.BatchSize = %d, want the env value 8 over the 16 in YAML", got)
	}
}

func TestLoadModelRolesFromEnvOnly(t *testing.T) {
	t.Setenv("TRELIS_MODELS_CHAT_BASE_URL", "http://localhost:4000/v1")
	t.Setenv("TRELIS_MODELS_CHAT_MODEL", "local")

	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Models.Chat.Configured() || cfg.Models.Chat.Model != "local" {
		t.Errorf("Models.Chat = %+v, want the env values", cfg.Models.Chat)
	}
	if cfg.Models.Judge.Configured() {
		t.Errorf("Models.Judge = %+v, want unconfigured", cfg.Models.Judge)
	}
}

func TestLoadResponsesProtocolFromEnv(t *testing.T) {
	t.Setenv("TRELIS_MODELS_CHAT_BASE_URL", "http://localhost:4000/v1")
	t.Setenv("TRELIS_MODELS_CHAT_MODEL", "local")
	t.Setenv("TRELIS_MODELS_CHAT_PROTOCOL", "openai-responses")

	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models.Chat.Protocol != api.APIOpenAIResponses {
		t.Errorf("Models.Chat.Protocol = %q, want %q", cfg.Models.Chat.Protocol, api.APIOpenAIResponses)
	}
}

// The rejection has to say which protocols this build does ship, or an
// operator cannot tell a typo from an unsupported provider.
func TestLoadRejectsUnknownProtocolNamingBoth(t *testing.T) {
	t.Setenv("TRELIS_MODELS_CHAT_PROTOCOL", "grpc")

	_, err := Load(writeConfig(t, modelsYAML))
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	for _, want := range []string{string(api.APIOpenAICompletions), string(api.APIOpenAIResponses)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestLoadIgnoresAKeyWithoutAProvider(t *testing.T) {
	t.Setenv("TRELIS_MODELS_JUDGE_API_KEY", "sk-left-over")

	cfg, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Models.Judge.Configured() {
		t.Errorf("Models.Judge = %+v, want unconfigured with only a key", cfg.Models.Judge)
	}
}

func TestLoadRejectsInvalidModelRoles(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantMsg string
	}{
		{"model without base url", map[string]string{"TRELIS_MODELS_CHAT_BASE_URL": " "}, "models.chat.base_url"},
		{"base url without model", map[string]string{"TRELIS_MODELS_JUDGE_MODEL": " "}, "models.judge.model"},
		{"base url not http", map[string]string{"TRELIS_MODELS_EXTRACTION_BASE_URL": "litellm:4000"}, "models.extraction.base_url"},
		{"unknown protocol", map[string]string{"TRELIS_MODELS_CHAT_PROTOCOL": "grpc"}, "models.chat.protocol"},
		{"embedding model without base url", map[string]string{"TRELIS_MODELS_EMBEDDING_BASE_URL": " "}, "models.embedding.base_url"},
		{"embedding batch size zero", map[string]string{"TRELIS_MODELS_EMBEDDING_BATCH_SIZE": "0"}, "models.embedding.batch_size"},
		{"embedding batch size negative", map[string]string{"TRELIS_MODELS_EMBEDDING_BATCH_SIZE": "-4"}, "models.embedding.batch_size"},
		{"rerank model without base url", map[string]string{"TRELIS_MODELS_RERANK_BASE_URL": " "}, "models.rerank.base_url"},
		{"rerank base url without model", map[string]string{"TRELIS_MODELS_RERANK_MODEL": " "}, "models.rerank.model"},
		{"rerank base url not http", map[string]string{"TRELIS_MODELS_RERANK_BASE_URL": "jina.ai"}, "models.rerank.base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(writeConfig(t, modelsYAML))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestLoadRejectsInvalidDependencies(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantMsg string
	}{
		{"empty postgres host", map[string]string{"TRELIS_POSTGRES_HOST": " "}, "postgres.host"},
		{"postgres port zero", map[string]string{"TRELIS_POSTGRES_PORT": "0"}, "postgres.port"},
		{"postgres port too big", map[string]string{"TRELIS_POSTGRES_PORT": "70000"}, "postgres.port"},
		{"postgres port not a number", map[string]string{"TRELIS_POSTGRES_PORT": "abc"}, "postgres.port"},
		{"empty postgres dbname", map[string]string{"TRELIS_POSTGRES_DBNAME": " "}, "postgres.dbname"},
		{"empty postgres user", map[string]string{"TRELIS_POSTGRES_USER": " "}, "postgres.user"},
		{"bad sslmode", map[string]string{"TRELIS_POSTGRES_SSLMODE": "maybe"}, "postgres.sslmode"},
		{"empty neo4j host", map[string]string{"TRELIS_NEO4J_HOST": " "}, "neo4j.host"},
		{"neo4j port too big", map[string]string{"TRELIS_NEO4J_PORT": "99999"}, "neo4j.port"},
		{"empty temporal host", map[string]string{"TRELIS_TEMPORAL_HOST": " "}, "temporal.host"},
		{"temporal port zero", map[string]string{"TRELIS_TEMPORAL_PORT": "0"}, "temporal.port"},
		{"empty temporal namespace", map[string]string{"TRELIS_TEMPORAL_NAMESPACE": " "}, "temporal.namespace"},
		{"empty bucket", map[string]string{"TRELIS_STORAGE_S3_BUCKET": " "}, "storage.s3.bucket"},
		{"empty region", map[string]string{"TRELIS_STORAGE_S3_REGION": " "}, "storage.s3.region"},
		{"endpoint not a URL", map[string]string{"TRELIS_STORAGE_S3_ENDPOINT": "localhost:9000"}, "storage.s3.endpoint"},
		{"access key without secret", map[string]string{"TRELIS_STORAGE_S3_ACCESS_KEY_ID": "trelis"}, "storage.s3.secret_access_key"},
		{"storage max size zero", map[string]string{"TRELIS_STORAGE_MAX_FILE_SIZE_MB": "0"}, "storage.max_file_size_mb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load(writeConfig(t, depsYAML))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestS3StorageIsReadFromEnv(t *testing.T) {
	for k, v := range map[string]string{
		"TRELIS_STORAGE_S3_ENDPOINT":          "http://silo:9000",
		"TRELIS_STORAGE_S3_BUCKET":            "docs",
		"TRELIS_STORAGE_S3_ACCESS_KEY_ID":     "trelis",
		"TRELIS_STORAGE_S3_SECRET_ACCESS_KEY": "s3cr3t-dev",
		"TRELIS_STORAGE_S3_FORCE_PATH_STYLE":  "true",
		"TRELIS_STORAGE_S3_CREATE_BUCKET":     "true",
	} {
		t.Setenv(k, v)
	}
	cfg, err := Load(writeConfig(t, depsYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := S3{Endpoint: "http://silo:9000", Region: "us-east-1", Bucket: "docs", AccessKeyID: "trelis", SecretAccessKey: "s3cr3t-dev", ForcePathStyle: true, CreateBucket: true}
	if cfg.Storage.S3 != want {
		t.Errorf("storage.s3 = %+v, want %+v", cfg.Storage.S3, want)
	}
}
