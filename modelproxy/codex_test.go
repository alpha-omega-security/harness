package modelproxy

import (
	"slices"
	"strings"
	"testing"
)

func TestBaseURL(t *testing.T) {
	for p, want := range map[*Provider]string{
		&Anthropic: "http://proxy:1/model-proxy",
		&OpenAI:    "http://proxy:1/model-proxy/v1",
		&ChatGPT:   "http://proxy:1/model-proxy/backend-api/codex",
	} {
		if got := p.BaseURL("http://proxy:1"); got != want {
			t.Errorf("BaseURL() = %q, want %q", got, want)
		}
	}
}

func TestCodexConfigWithAnAPIKey(t *testing.T) {
	config, env, err := CodexConfig(OpenAI, "http://proxy:1", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`model_provider = "modelproxy"`,
		`chatgpt_base_url = "http://proxy:1/model-proxy/backend-api"`,
		`base_url = "http://proxy:1/model-proxy/v1"`,
		`env_key = "CODEX_API_KEY"`,
		`supports_websockets = false`,
		`request_max_retries = 0`,
	} {
		if !strings.Contains(string(config), want) {
			t.Errorf("config misses %q:\n%s", want, config)
		}
	}
	if strings.Contains(string(config), "requires_openai_auth") || env != nil {
		t.Errorf("an API key config signs in with ChatGPT: env = %v\n%s", env, config)
	}
	if !strings.HasPrefix(string(config), "model_provider") {
		t.Errorf("the config must start with its top-level keys:\n%s", config)
	}
}

func TestCodexConfigWithAnAccessToken(t *testing.T) {
	config, env, err := CodexConfig(ChatGPT, "https://proxy:1", "/harness-state/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "requires_openai_auth = true") || strings.Contains(string(config), "env_key") {
		t.Errorf("config:\n%s", config)
	}
	if !strings.Contains(string(config), `base_url = "https://proxy:1/model-proxy/backend-api/codex"`) {
		t.Errorf("config:\n%s", config)
	}
	want := []string{"CODEX_CA_CERTIFICATE=/harness-state/ca.pem", "CODEX_AUTHAPI_BASE_URL=https://proxy:1/model-proxy/auth"}
	if !slices.Equal(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
}

func TestCodexConfigRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		p              Provider
		origin, caFile string
	}{
		"anthropic":          {Anthropic, "http://proxy:1", ""},
		"access token no CA": {ChatGPT, "https://proxy:1", ""},
		"access token HTTP":  {ChatGPT, "http://proxy:1", "/ca.pem"},
		"quoted origin":      {OpenAI, `http://proxy:1"` + "\nmodel_provider = \"openai", ""},
		"quoted CA":          {ChatGPT, "https://proxy:1", `/ca"`},
		"control character":  {OpenAI, "http://proxy:1\x01", ""},
	} {
		if config, _, err := CodexConfig(tc.p, tc.origin, tc.caFile); err == nil {
			t.Errorf("%s: config accepted:\n%s", name, config)
		}
	}
}
