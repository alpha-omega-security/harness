package modelproxy

import (
	"errors"
	"fmt"
	"strings"
)

// CodexConfig returns the config.toml that sends Codex's model calls to a proxy for p served at origin, and
// the environment Codex needs with it. With ChatGPT, Codex signs in through the proxy over HTTPS and trusts
// the CA written to caFile, a path inside the container. Top-level keys a caller adds go before the
// returned config, which ends with tables.
func CodexConfig(p Provider, origin, caFile string) ([]byte, []string, error) {
	if p.ClientPath == "" {
		return nil, nil, errors.New("modelproxy: Codex only reaches the OpenAI and ChatGPT providers")
	}
	for _, value := range []string{origin, caFile} {
		if strings.ContainsFunc(value, func(r rune) bool { return r == '"' || r == '\\' || r < 0x20 || r == 0x7f }) {
			return nil, nil, fmt.Errorf("modelproxy: %q cannot go in config.toml", value)
		}
	}
	auth := `env_key = "CODEX_API_KEY"`
	var env []string
	if p.codexAccessToken {
		if caFile == "" {
			return nil, nil, errors.New("modelproxy: Codex needs the proxy CA to sign in through it")
		}
		if !strings.HasPrefix(origin, "https://") {
			return nil, nil, errors.New("modelproxy: Codex signs in through the proxy only over HTTPS")
		}
		auth = "requires_openai_auth = true"
		env = []string{"CODEX_CA_CERTIFICATE=" + caFile, "CODEX_AUTHAPI_BASE_URL=" + origin + PathPrefix + "/auth"}
	}
	// Web search and the ChatGPT features below call tools and routes the proxy refuses. Retries would spend
	// the token's request budget on a request the proxy or the upstream already refused.
	config := fmt.Sprintf(`model_provider = "modelproxy"
web_search = "disabled"
chatgpt_base_url = %q

[features]
remote_plugin = false
plugin_sharing = false
goals = false
image_generation = false
skill_mcp_dependency_install = false

[model_providers.modelproxy]
name = "modelproxy"
base_url = %q
%s
wire_api = "responses"
supports_websockets = false
supports_standalone_web_search = false
request_max_retries = 0
stream_max_retries = 0
`, origin+PathPrefix+"/backend-api", p.BaseURL(origin), auth)
	return []byte(config), env, nil
}
