package modelproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type upstreamCall struct {
	method, path, apiKey, auth, cookie, accountID, body string
}

// setup returns a proxy for p, pointed at a fake upstream that records each call.
func setup(t *testing.T, p Provider, status int) (*httptest.Server, *[]upstreamCall, string) {
	t.Helper()
	return setupWith(t, p, status, Options{})
}

func setupWith(t *testing.T, p Provider, status int, opts Options) (*httptest.Server, *[]upstreamCall, string) {
	t.Helper()
	var calls []upstreamCall
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, upstreamCall{r.Method, r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("ChatGPT-Account-Id"), string(body)})
		if status == http.StatusFound {
			w.Header().Set("Location", "https://evil.example/")
		}
		w.Header().Set("Set-Cookie", "session=1")
		w.WriteHeader(status)
		response := `{"ok":true}`
		if strings.HasSuffix(r.URL.Path, "/v1/user-auth-credential/whoami") {
			response = `{"email":"security@example.com","chatgpt_user_id":"user-secret","chatgpt_account_id":"account-secret","chatgpt_plan_type":"enterprise","chatgpt_account_is_fedramp":false}`
		}
		if strings.HasSuffix(r.URL.Path, "/wham/config/bundle") {
			response = `{"config_toml":{"enterprise_managed":[{"id":"secret","name":"secret","contents":"additional_developer_instructions = \\"internal\\""}]}}`
		}
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(up.Close)
	p.Upstream = up.URL
	upstreams := map[string]string{}
	for prefix, raw := range p.Upstreams {
		u, _ := url.Parse(raw)
		upstreams[prefix] = up.URL + u.Path
	}
	if p.Upstreams != nil {
		p.Upstreams = upstreams
	}

	mp, err := New(p, "real-key", "allowed", opts)
	if err != nil {
		t.Fatal(err)
	}
	token, revoke := mp.Issue(t.Context())
	srv := httptest.NewServer(mp)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { revoke() })

	return srv, &calls, token
}

func call(t *testing.T, srv *httptest.Server, method, path string, header http.Header) *http.Response {
	t.Helper()
	if method != http.MethodPost {
		return callBody(t, srv, method, path, "", header)
	}
	return callBody(t, srv, method, path, `{"model":"allowed"}`, header)
}

func callBody(t *testing.T, srv *httptest.Server, method, path, body string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func withKey(name, value string) http.Header {
	h := http.Header{}
	h.Set(name, value)
	h.Set("Cookie", "secret=1")

	return h
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

func wantStatus(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s: status = %d, want %d", what, resp.StatusCode, want)
	}
}

func TestAnthropicSwapsTheToken(t *testing.T) {
	srv, calls, token := setup(t, Anthropic, http.StatusOK)

	resp := call(t, srv, http.MethodPost, PathPrefix+"/v1/messages?beta=true", withKey("X-Api-Key", token))

	wantStatus(t, resp, http.StatusOK, "messages")
	if got := resp.Header.Get("Set-Cookie"); got != "" {
		t.Errorf("Set-Cookie = %q, want it stripped", got)
	}
	if len(*calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.path != "/v1/messages" || got.apiKey != "real-key" || got.auth != "" || got.cookie != "" {
		t.Errorf("upstream call = %+v", got)
	}
}

func TestOpenAISwapsTheBearerToken(t *testing.T) {
	srv, calls, token := setup(t, OpenAI, http.StatusOK)

	body := `{"model":"allowed","input":[{"type":"message","id":"msg_current","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	resp := callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", body, withKey("Authorization", "Bearer "+token))

	wantStatus(t, resp, http.StatusOK, "responses")
	if len(*calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.auth != "Bearer real-key" || got.apiKey != "" {
		t.Errorf("upstream credentials = %q, %q", got.auth, got.apiKey)
	}
	for _, want := range []string{`"store":false`, `"background":false`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("unsafe OpenAI defaults were not overridden: %s", got.body)
		}
	}
}

func TestChatGPTSwapsTheAccessTokenOnBothUpstreams(t *testing.T) {
	srv, calls, token := setup(t, ChatGPT, http.StatusOK)
	if !strings.HasPrefix(token, "at-harness-") {
		t.Fatalf("Codex only reads a token starting with at- as an access token: %q", token)
	}

	whoami := call(t, srv, http.MethodGet, PathPrefix+"/auth/v1/user-auth-credential/whoami", withKey("Authorization", "Bearer "+token))
	identity := readAll(t, whoami)
	backendKey := withKey("Authorization", "Bearer "+token)
	backendKey.Set("ChatGPT-Account-Id", "attacker-controlled")
	bundle := call(t, srv, http.MethodGet, PathPrefix+"/backend-api/wham/config/bundle", backendKey)
	managed := readAll(t, bundle)
	responses := call(t, srv, http.MethodPost, PathPrefix+"/backend-api/codex/responses", backendKey)

	wantStatus(t, whoami, http.StatusOK, "whoami")
	wantStatus(t, bundle, http.StatusOK, "bundle")
	wantStatus(t, responses, http.StatusOK, "responses")
	if len(*calls) != 3 {
		t.Fatalf("upstream calls = %d, want 3", len(*calls))
	}
	for i, want := range []string{"/api/accounts/v1/user-auth-credential/whoami", "/backend-api/wham/config/bundle", "/backend-api/codex/responses"} {
		got := (*calls)[i]
		if got.path != want || got.auth != "Bearer real-key" || got.cookie != "" {
			t.Errorf("upstream call %d = %+v", i, got)
		}
	}
	if (*calls)[0].accountID != "" || (*calls)[1].accountID != "account-secret" || (*calls)[2].accountID != "account-secret" {
		t.Errorf("workspace identity was not applied server-side only: %+v", *calls)
	}
	if strings.Contains(identity, "secret") || strings.Contains(identity, "security@example.com") || !strings.Contains(identity, `"chatgpt_account_id":"harness"`) {
		t.Errorf("private workspace response reached the agent: %s", identity)
	}
	if strings.Contains(managed, "internal") || !strings.Contains(managed, `"enterprise_managed":[]`) {
		t.Errorf("private workspace response reached the agent: %s", managed)
	}
	body := (*calls)[2].body
	if !strings.Contains(body, `"store":false`) || strings.Contains(body, "background") || strings.Contains(body, "max_output_tokens") {
		t.Errorf("the ChatGPT backend only accepts store set to false: %s", body)
	}
}

func TestChatGPTRefusesAnIdentityWithoutAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"email":"security@example.com","chatgpt_plan_type":"enterprise"}`)
	}))
	t.Cleanup(up.Close)
	p := ChatGPT
	p.Upstreams = map[string]string{"/auth": up.URL + "/api/accounts", "/backend-api": up.URL + "/backend-api"}
	mp, err := New(p, "real-key", "allowed", Options{})
	if err != nil {
		t.Fatal(err)
	}
	token, revoke := mp.Issue(t.Context())
	t.Cleanup(func() { revoke() })
	srv := httptest.NewServer(mp)
	t.Cleanup(srv.Close)

	resp := call(t, srv, http.MethodGet, PathPrefix+"/auth/v1/user-auth-credential/whoami", withKey("Authorization", "Bearer "+token))

	wantStatus(t, resp, http.StatusBadGateway, "whoami")
	if body := readAll(t, resp); strings.Contains(body, "security@example.com") {
		t.Errorf("identity reached the agent: %s", body)
	}
}

func TestChatGPTRefusesTheOtherBackendRoutes(t *testing.T) {
	srv, calls, token := setup(t, ChatGPT, http.StatusOK)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/backend-api/codex/models?client_version=0.163.0&extra=1"},
		{http.MethodGet, "/backend-api/codex/models?model=other"},
		{http.MethodPost, "/backend-api/codex/responses?client_version=0.163.0"},
		{http.MethodGet, "/backend-api/wham/accounts/check"},
		{http.MethodPost, "/backend-api/ps/mcp"},
		{http.MethodPost, "/auth/v1/user-auth-credential/whoami"},
		{http.MethodGet, "/backend-api/codex/responses"},
		{http.MethodPost, "/v1/responses"},
	} {
		if got := call(t, srv, tc.method, PathPrefix+tc.path, withKey("Authorization", "Bearer "+token)).StatusCode; got != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, got, http.StatusForbidden)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("refused routes reached the upstream: %+v", *calls)
	}
}

func TestRefusals(t *testing.T) {
	srv, calls, token := setup(t, Anthropic, http.StatusOK)

	for name, tc := range map[string]struct {
		method, path string
		header       http.Header
		status       int
	}{
		"no token":       {http.MethodPost, PathPrefix + "/v1/messages", http.Header{}, http.StatusUnauthorized},
		"unknown token":  {http.MethodPost, PathPrefix + "/v1/messages", withKey("X-Api-Key", "harness-guess"), http.StatusUnauthorized},
		"real key":       {http.MethodPost, PathPrefix + "/v1/messages", withKey("X-Api-Key", "real-key"), http.StatusUnauthorized},
		"other route":    {http.MethodPost, PathPrefix + "/v1/files", withKey("X-Api-Key", token), http.StatusForbidden},
		"wrong method":   {http.MethodDelete, PathPrefix + "/v1/messages", withKey("X-Api-Key", token), http.StatusForbidden},
		"traversal":      {http.MethodPost, PathPrefix + "/v1/x/../messages", withKey("X-Api-Key", token), http.StatusForbidden},
		"encoded path":   {http.MethodPost, PathPrefix + "/v1/messages%2Fbatches", withKey("X-Api-Key", token), http.StatusForbidden},
		"nested model":   {http.MethodGet, PathPrefix + "/v1/models/a/b", withKey("X-Api-Key", token), http.StatusForbidden},
		"query":          {http.MethodPost, PathPrefix + "/v1/messages?feature=remote", withKey("X-Api-Key", token), http.StatusForbidden},
		"outside prefix": {http.MethodPost, "/v1/messages", withKey("X-Api-Key", token), http.StatusNotFound},
		"openai route":   {http.MethodPost, PathPrefix + "/v1/responses", withKey("X-Api-Key", token), http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			wantStatus(t, call(t, srv, tc.method, tc.path, tc.header), tc.status, name)
		})
	}
	if len(*calls) != 0 {
		t.Errorf("nothing should reach the upstream: %+v", *calls)
	}
}

func TestRevokedTokenIsRefused(t *testing.T) {
	mp, err := New(Anthropic, "real-key", "allowed", Options{})
	if err != nil {
		t.Fatal(err)
	}
	token, revoke := mp.Issue(t.Context())
	srv := httptest.NewServer(mp)
	t.Cleanup(srv.Close)

	revoke()

	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", withKey("X-Api-Key", token)), http.StatusUnauthorized, "revoked")
}

func TestUpstreamRedirectIsNotRelayed(t *testing.T) {
	srv, _, token := setup(t, Anthropic, http.StatusFound)

	resp := call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", withKey("X-Api-Key", token))

	wantStatus(t, resp, http.StatusBadGateway, "redirect")
	if got := resp.Header.Get("Location"); got != "" {
		t.Errorf("Location = %q, want none", got)
	}
}

func TestNewRefusesInvalidArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		provider   Provider
		key, model string
		opts       Options
	}{
		"no key":               {Anthropic, "", "allowed", Options{}},
		"no model":             {Anthropic, "real-key", "", Options{}},
		"negative request cap": {Anthropic, "real-key", "allowed", Options{MaxRequests: -1}},
		"no routes":            {Provider{Upstream: Anthropic.Upstream, SetKey: Anthropic.SetKey}, "real-key", "allowed", Options{}},
		"no key setter":        {Provider{Upstream: Anthropic.Upstream, Routes: Anthropic.Routes}, "real-key", "allowed", Options{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(tc.provider, tc.key, tc.model, tc.opts); err == nil {
				t.Error("New succeeded")
			}
		})
	}
}

func TestBodyOfAnotherMethodIsRefused(t *testing.T) {
	srv, calls, token := setup(t, ChatGPT, http.StatusOK)
	route := PathPrefix + "/backend-api/codex/models?client_version=0.163.0"

	wantStatus(t, callBody(t, srv, http.MethodGet, route, `{"tools":[{"type":"mcp"}]}`, withKey("Authorization", "Bearer "+token)), http.StatusForbidden, "GET with a body")
	if len(*calls) != 0 {
		t.Fatalf("an unfiltered body reached the upstream: %+v", *calls)
	}
	wantStatus(t, call(t, srv, http.MethodGet, route, withKey("Authorization", "Bearer "+token)), http.StatusOK, "GET without a body")
}

func TestCallerRequestIsLeftUnchanged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(up.Close)
	provider := Anthropic
	provider.Upstream = up.URL
	mp, err := New(provider, "real-key", "allowed", Options{})
	if err != nil {
		t.Fatal(err)
	}
	token, revoke := mp.Issue(t.Context())
	t.Cleanup(func() { revoke() })
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mp.ServeHTTP(w, r)
		paths = append(paths, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", withKey("X-Api-Key", token)), http.StatusOK, "forwarded")
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/files", withKey("X-Api-Key", token)), http.StatusForbidden, "refused")
	if len(paths) != 2 || paths[0] != PathPrefix+"/v1/messages" || paths[1] != PathPrefix+"/v1/files" {
		t.Errorf("caller paths after ServeHTTP = %v", paths)
	}
}

func TestProviderCapabilitiesAndOtherModelsAreRefused(t *testing.T) {
	srv, calls, token := setup(t, OpenAI, http.StatusOK)
	for name, body := range map[string]string{
		"other model":             `{"model":"expensive"}`,
		"remote MCP":              `{"model":"allowed","tools":[{"type":"mcp","server_url":"https://evil.example"}]}`,
		"remote file":             `{"model":"allowed","input":[{"type":"input_file","file_url":"https://evil.example"}]}`,
		"conversation":            `{"model":"allowed","conversation":"conv_other"}`,
		"item reference":          `{"model":"allowed","input":[{"type":"item_reference","id":"item_other"}]}`,
		"implicit item reference": `{"model":"allowed","input":[{"id":"item_other"}]}`,
		"null item reference":     `{"model":"allowed","input":[{"type":null,"id":"item_other"}]}`,
		"multi-agent":             `{"model":"allowed","multi_agent":{"enabled":true,"max_concurrent_subagents":100}}`,
		"nested multi-agent item": `{"model":"allowed","input":[{"type":"multi_agent_call","name":"spawn_agent"}]}`,
		"untyped tool":            `{"model":"allowed","tools":[{"name":"remote"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", body, withKey("Authorization", "Bearer "+token)), http.StatusForbidden, body)
		})
	}
	if len(*calls) != 0 {
		t.Errorf("nothing should reach the upstream: %+v", *calls)
	}
}

func TestAnthropicFallbacksAreRefused(t *testing.T) {
	srv, calls, token := setup(t, Anthropic, http.StatusOK)
	body := `{"model":"allowed","max_tokens":16000,"fallbacks":[{"model":"expensive","max_tokens":128000}]}`
	header := withKey("X-Api-Key", token)
	header.Set("Anthropic-Beta", "server-side-fallback-2026-07-01")

	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/messages?beta=true", body, header), http.StatusForbidden, "fallbacks")
	if len(*calls) != 0 {
		t.Errorf("the fallback reached the upstream: %+v", *calls)
	}
}

func TestProviderTokenLimitIsClamped(t *testing.T) {
	srv, calls, token := setup(t, OpenAI, http.StatusOK)
	resp := callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", `{"model":"allowed","max_output_tokens":16001}`, withKey("Authorization", "Bearer "+token))
	wantStatus(t, resp, http.StatusOK, "responses")
	if len(*calls) != 1 || !strings.Contains((*calls)[0].body, `"max_output_tokens":16000`) {
		t.Errorf("upstream calls = %+v, want one clamped to 16000", *calls)
	}
}

func TestEveryAccessTokenRequestSpendsTheQuota(t *testing.T) {
	srv, calls, token := setup(t, ChatGPT, http.StatusOK)
	whoami := PathPrefix + "/auth/v1/user-auth-credential/whoami"
	for range defaultMaxRequests {
		call(t, srv, http.MethodGet, whoami, withKey("Authorization", "Bearer "+token))
	}
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/backend-api/codex/responses", withKey("Authorization", "Bearer "+token)), http.StatusUnauthorized, "request after token checks")
	wantStatus(t, call(t, srv, http.MethodGet, whoami, withKey("Authorization", "Bearer at-harness-guess")), http.StatusUnauthorized, "token check with an unknown token")
	if len(*calls) != defaultMaxRequests {
		t.Errorf("upstream calls = %d, want %d", len(*calls), defaultMaxRequests)
	}
}

func TestProviderRequestQuota(t *testing.T) {
	srv, calls, token := setup(t, Anthropic, http.StatusOK)
	for i := range defaultMaxRequests {
		if got := call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", withKey("X-Api-Key", token)).StatusCode; got != http.StatusOK {
			t.Fatalf("request %d: status = %d", i+1, got)
		}
	}
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", withKey("X-Api-Key", token)), http.StatusUnauthorized, "request after quota")
	if len(*calls) != defaultMaxRequests {
		t.Errorf("upstream calls = %d, want %d", len(*calls), defaultMaxRequests)
	}
}

func TestMaxRequestsReplacesTheQuota(t *testing.T) {
	srv, calls, token := setupWith(t, Anthropic, http.StatusOK, Options{MaxRequests: 2})
	for i := range 2 {
		wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", withKey("X-Api-Key", token)), http.StatusOK, "request "+strconv.Itoa(i+1))
	}
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", withKey("X-Api-Key", token)), http.StatusUnauthorized, "request after quota")
	if len(*calls) != 2 {
		t.Errorf("upstream calls = %d, want 2", len(*calls))
	}
}

func TestNestedToolsAndUnknownProtocolCapabilitiesFailClosed(t *testing.T) {
	for _, provider := range []Provider{OpenAI, ChatGPT} {
		t.Run(provider.TokenPrefix, func(t *testing.T) {
			for _, body := range []string{
				`{"model":"allowed","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"shell","environment":{"type":"container_auto","network_policy":{"type":"allowlist","allowed_domains":["attacker.invalid"]}}}]}]}`,
				`{"model":"allowed","input":[{"type":"tool_search_output","tools":[{"type":"namespace","name":"n","tools":[{"type":"shell"}]}]}]}`,
				`{"model":"allowed","tools":[{"type":"namespace","name":"n","tools":[{"type":"mcp","server_url":"https://attacker.invalid"}]}]}`,
				`{"model":"allowed","tools":[{"type":"tool_search","execution":"server"}]}`,
				`{"model":"allowed","input":[{"type":"configuration_update","tools":[]}]}`,
				`{"model":"allowed","input":[{"type":"future_provider_tool"}]}`,
				`{"model":"allowed","future_provider_option":true}`,
				`{"model":"allowed","background":true}`,
			} {
				srv, calls, token := setup(t, provider, http.StatusOK)
				route := "/v1/responses"
				if provider.PrivateWorkspace {
					route = "/backend-api/codex/responses"
				}
				wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+route, body, withKey("Authorization", "Bearer "+token)), http.StatusForbidden, "unsafe request reached upstream: "+body)
				if len(*calls) != 0 {
					t.Fatalf("unsafe request reached upstream: %s", body)
				}
			}
		})
	}
}

func TestLocalToolConversationPasses(t *testing.T) {
	srv, calls, token := setup(t, OpenAI, http.StatusOK)
	body := `{"model":"allowed","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"type":{"type":"string"}}}}]},{"type":"tool_search","execution":"client"}],"input":[{"role":"user","content":[{"type":"input_text","text":"report"}]},{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]},{"type":"function_call","call_id":"c","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"source"},{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"local","format":{"type":"text"}}]}],"include":["reasoning.encrypted_content"]}`
	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", body, withKey("Authorization", "Bearer "+token)), http.StatusOK, "local tools rejected")
	if len(*calls) != 1 {
		t.Errorf("local tools rejected: upstream calls = %d", len(*calls))
	}
}

func TestTokensShareTheChargeAndStopOnCancellation(t *testing.T) {
	var mu sync.Mutex
	var forwarded []int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		forwarded = append(forwarded, len(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	provider := OpenAI
	provider.Upstream = upstream.URL
	type issuedKey struct{}
	var charged []int
	mp, err := New(provider, "key", "allowed", Options{Charge: func(ctx context.Context, bytes int) bool {
		if ctx.Value(issuedKey{}) != "run" {
			t.Error("the charge did not get the context the token was issued with")
		}
		mu.Lock()
		defer mu.Unlock()
		charged = append(charged, bytes)
		return len(charged) == 1
	}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mp)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), issuedKey{}, "run"))
	defer cancel()
	a, revokeA := mp.Issue(ctx)
	defer revokeA()
	b, revokeB := mp.Issue(ctx)
	defer revokeB()

	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/responses", withKey("Authorization", "Bearer "+a)), http.StatusOK, "first token")
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/responses", withKey("Authorization", "Bearer "+b)), http.StatusTooManyRequests, "second token")
	cancel()
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/responses", withKey("Authorization", "Bearer "+a)), http.StatusUnauthorized, "after cancellation")

	mu.Lock()
	defer mu.Unlock()
	if len(forwarded) != 1 || len(charged) != 2 || charged[0] != forwarded[0] || charged[1] != forwarded[0] {
		t.Errorf("charged %v, forwarded %v: want each request charged the size of the filtered body", charged, forwarded)
	}
}

func TestAnthropicLocalToolsAndContextEdits(t *testing.T) {
	srv, calls, token := setup(t, Anthropic, http.StatusOK)
	body := `{"model":"allowed","messages":[{"role":"user","content":[{"type":"text","text":"report"}]},{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{"path":"a.c"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"source"},{"type":"tool_reference","tool_name":"Read"}]}]}],"tools":[{"name":"Read","description":"read","defer_loading":true,"input_schema":{"type":"object"}}],"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}}`
	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/messages", body, withKey("X-Api-Key", token)), http.StatusOK, "valid local Anthropic conversation rejected")
	if len(*calls) != 1 {
		t.Fatalf("valid local Anthropic conversation rejected: upstream calls = %d", len(*calls))
	}
	body = strings.ReplaceAll(body, "clear_thinking_20251015", "future_remote_compaction")
	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/messages", body, withKey("X-Api-Key", token)), http.StatusForbidden, "unknown context capability reached Anthropic")
	if len(*calls) != 1 {
		t.Errorf("unknown context capability reached Anthropic: upstream calls = %d", len(*calls))
	}
}

func TestCapabilityHeadersAreNotForwarded(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone(); w.WriteHeader(http.StatusOK) }))
	defer up.Close()
	provider := Anthropic
	provider.Upstream = up.URL
	mp, err := New(provider, "key", "allowed", Options{})
	if err != nil {
		t.Fatal(err)
	}
	token, revoke := mp.Issue(t.Context())
	defer revoke()
	srv := httptest.NewServer(mp)
	defer srv.Close()
	h := withKey("X-Api-Key", token)
	h.Set("Anthropic-Beta", "context-management-2025-06-27,future-hosted-tools-2026-01-01")
	h.Set("OpenAI-Beta", "agents=enabled")
	h.Set("X-Unknown-Provider-Capability", "enabled")
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", h), http.StatusOK, "messages")
	if got.Get("Anthropic-Beta") != "context-management-2025-06-27" || got.Get("OpenAI-Beta") != "" || got.Get("X-Unknown-Provider-Capability") != "" {
		t.Errorf("unreviewed capability header reached upstream: %v", got)
	}
}

// Each reviewed protocol position passes, and one unreviewed field or shape at that position is refused.
func TestRequestShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider Provider
		accepted map[string]string
		refused  map[string]string
	}{
		{
			name:     "anthropic",
			provider: Anthropic,
			accepted: map[string]string{
				"thinking":        `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"redacted_thinking","data":"d"}]}]}`,
				"effort":          `{"model":"allowed","messages":[{"role":"user","content":"x","output_config":{"effort":"high"}}]}`,
				"clear tool uses": `{"model":"allowed","context_management":{"edits":[{"type":"clear_tool_uses_20250919","keep":{"type":"tool_uses","value":3}}]}}`,
				"system blocks":   `{"model":"allowed","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`,
				"direct tool use": `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{},"caller":{"type":"direct"}}]}]}`,
			},
			refused: map[string]string{
				"tools object":             `{"model":"allowed","tools":{}}`,
				"tool string":              `{"model":"allowed","tools":["Read"]}`,
				"hosted tool":              `{"model":"allowed","tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
				"tool field":               `{"model":"allowed","tools":[{"name":"Read","input_schema":{},"mcp_server":"s"}]}`,
				"messages object":          `{"model":"allowed","messages":{}}`,
				"message field":            `{"model":"allowed","messages":[{"role":"user","content":"x","container":"c"}]}`,
				"output config string":     `{"model":"allowed","messages":[{"role":"user","content":"x","output_config":"high"}]}`,
				"output config field":      `{"model":"allowed","messages":[{"role":"user","content":"x","output_config":{"effort":"high","task_budget":1}}]}`,
				"system object":            `{"model":"allowed","system":{}}`,
				"system string block":      `{"model":"allowed","system":["s"]}`,
				"text field":               `{"model":"allowed","system":[{"type":"text","text":"s","citations":[]}]}`,
				"thinking field":           `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s","source":"x"}]}]}`,
				"tool use field":           `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{},"container":"c"}]}]}`,
				"tool use caller string":   `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{},"caller":"direct"}]}]}`,
				"tool use caller untyped":  `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{},"caller":{}}]}]}`,
				"tool use caller field":    `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{},"caller":{"type":"direct","tool_id":"s"}}]}]}`,
				"hosted tool use caller":   `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{},"caller":{"type":"code_execution_20260120","tool_id":"s"}}]}]}`,
				"tool result field":        `{"model":"allowed","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","container":"c"}]}]}`,
				"tool reference field":     `{"model":"allowed","messages":[{"role":"user","content":[{"type":"tool_reference","tool_name":"Read","server":"s"}]}]}`,
				"server tool block":        `{"model":"allowed","messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{}}]}]}`,
				"context management list":  `{"model":"allowed","context_management":[]}`,
				"context management field": `{"model":"allowed","context_management":{"edits":[],"compaction":true}}`,
				"edits object":             `{"model":"allowed","context_management":{"edits":{}}}`,
				"edit string":              `{"model":"allowed","context_management":{"edits":["clear_thinking_20251015"]}}`,
				"clear thinking field":     `{"model":"allowed","context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all","trigger":{}}]}}`,
				"clear tool uses field":    `{"model":"allowed","context_management":{"edits":[{"type":"clear_tool_uses_20250919","server":"s"}]}}`,
				"token limit string":       `{"model":"allowed","max_tokens":"16000"}`,
				"token limit zero":         `{"model":"allowed","max_tokens":0}`,
			},
		},
		{
			name:     "openai",
			provider: OpenAI,
			accepted: map[string]string{
				"string input":      `{"model":"allowed","input":"report"}`,
				"local tools":       `{"model":"allowed","tools":[{"type":"local_shell"},{"type":"apply_patch"},{"type":"custom","name":"c","format":{"type":"text"}}]}`,
				"named tool choice": `{"model":"allowed","tool_choice":{"type":"function","name":"read"}}`,
				"client search":     `{"model":"allowed","input":[{"type":"tool_search_call","call_id":"c","arguments":{},"execution":"client"},{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_text","text":"x"}]}]}`,
				"assistant output":  `{"model":"allowed","input":[{"type":"message","id":"m","role":"assistant","phase":"final_answer","status":"completed","content":[{"type":"output_text","text":"x","annotations":[],"logprobs":[]}]}]}`,
			},
			refused: map[string]string{
				"include string":          `{"model":"allowed","include":"reasoning.encrypted_content"}`,
				"include other":           `{"model":"allowed","include":["file_search_call.results"]}`,
				"function field":          `{"model":"allowed","tools":[{"type":"function","name":"f","server_url":"https://attacker.invalid"}]}`,
				"custom field":            `{"model":"allowed","tools":[{"type":"custom","name":"c","server_url":"https://attacker.invalid"}]}`,
				"local shell field":       `{"model":"allowed","tools":[{"type":"local_shell","environment":{}}]}`,
				"client search field":     `{"model":"allowed","tools":[{"type":"tool_search","execution":"client","server_url":"https://attacker.invalid"}]}`,
				"tool choice string":      `{"model":"allowed","tool_choice":"web_search"}`,
				"tool choice hosted":      `{"model":"allowed","tool_choice":{"type":"mcp","server_label":"s"}}`,
				"tool choice field":       `{"model":"allowed","tool_choice":{"type":"function","name":"f","server":"s"}}`,
				"input number":            `{"model":"allowed","input":1}`,
				"input string item":       `{"model":"allowed","input":["report"]}`,
				"message field":           `{"model":"allowed","input":[{"role":"user","content":"x","file_url":"https://attacker.invalid"}]}`,
				"message image":           `{"model":"allowed","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://attacker.invalid"}]}]}`,
				"output text field":       `{"model":"allowed","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x","file_url":"https://attacker.invalid"}]}]}`,
				"reasoning field":         `{"model":"allowed","input":[{"type":"reasoning","summary":[],"server":"s"}]}`,
				"function call field":     `{"model":"allowed","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}","server":"s"}]}`,
				"call output field":       `{"model":"allowed","input":[{"type":"function_call_output","call_id":"c","output":"x","server":"s"}]}`,
				"additional tools field":  `{"model":"allowed","input":[{"type":"additional_tools","tools":[],"server":"s"}]}`,
				"server tool search call": `{"model":"allowed","input":[{"type":"tool_search_call","call_id":"c","arguments":{},"execution":"server"}]}`,
				"tool search call field":  `{"model":"allowed","input":[{"type":"tool_search_call","call_id":"c","execution":"client","server":"s"}]}`,
			},
		},
	} {
		proxy, err := New(tc.provider, "key", "allowed", Options{})
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range tc.accepted {
			t.Run(tc.name+"/accepted/"+name, func(t *testing.T) {
				if _, err := proxy.safeBody([]byte(body)); err != nil {
					t.Error(err)
				}
			})
		}
		for name, body := range tc.refused {
			t.Run(tc.name+"/refused/"+name, func(t *testing.T) {
				if _, err := proxy.safeBody([]byte(body)); err == nil || err.Error() != "provider request not permitted" {
					t.Errorf("err = %v, want provider request not permitted", err)
				}
			})
		}
	}
}

func TestMalformedRequestsAreRefused(t *testing.T) {
	proxy, err := New(OpenAI, "key", "allowed", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"not JSON":      `model=allowed`,
		"trailing data": `{"model":"allowed"} {"model":"allowed","tools":[{"type":"mcp"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := proxy.safeBody([]byte(body)); err == nil || err.Error() != "invalid provider request" {
				t.Errorf("err = %v, want invalid provider request", err)
			}
		})
	}
	if _, err := proxy.safeBody([]byte(`{"model":"allowed","input":"` + strings.Repeat("x", maxBody) + `"}`)); !errors.Is(err, errRequestTooLarge) {
		t.Errorf("err = %v, want %v", err, errRequestTooLarge)
	}
}

func TestOversizedRequestIsRefusedBeforeTheUpstream(t *testing.T) {
	srv, calls, token := setup(t, OpenAI, http.StatusOK)
	body := `{"model":"allowed","input":"` + strings.Repeat("x", maxBody) + `"}`
	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", body, withKey("Authorization", "Bearer "+token)), http.StatusRequestEntityTooLarge, "oversized")
	if len(*calls) != 0 {
		t.Errorf("upstream calls = %d, want none", len(*calls))
	}
}

func TestMCPBetaHeaderIsRefused(t *testing.T) {
	var charged int
	srv, calls, token := setupWith(t, Anthropic, http.StatusOK, Options{Charge: func(context.Context, int) bool {
		charged++
		return true
	}})
	header := withKey("X-Api-Key", token)
	header.Set("Anthropic-Beta", "mcp-client-2025-04-04")
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/messages", header), http.StatusForbidden, "mcp beta")
	if len(*calls) != 0 || charged != 0 {
		t.Errorf("upstream calls = %d, charged = %d, want none", len(*calls), charged)
	}
}

// syncBuffer lets the proxy's handler goroutines log while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs() (*syncBuffer, Options) {
	logs := &syncBuffer{}

	return logs, Options{Log: slog.New(slog.NewTextHandler(logs, nil))}
}

func TestRefusalsAreLoggedWithoutTheConversation(t *testing.T) {
	logs, opts := captureLogs()
	srv, _, token := setupWith(t, OpenAI, http.StatusOK, opts)
	header := withKey("Authorization", "Bearer "+token)

	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", `{"model":"allowed","input":"SECRET-PAYLOAD"}`, header), http.StatusOK, "permitted")
	if got := logs.String(); got != "" {
		t.Fatalf("a permitted request was logged: %s", got)
	}

	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", `{"model":"allowed","input":[{"role":"user","content":[{"type":"input_text","text":"SECRET-PAYLOAD","future_field":true}]}]}`, header), http.StatusForbidden, "future field")
	wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/v1/models", header), http.StatusForbidden, "models")
	got := logs.String()
	for _, want := range []string{
		`msg="model proxy refused a request" route=/v1/responses shape="{input:[{content:[{future_field:bool text:string type=input_text}] role=user}] model:string}"`,
		`msg="model proxy refused a route" method=GET route=/v1/models`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("logs = %s, want %s", got, want)
		}
	}
	if strings.Contains(got, "SECRET-PAYLOAD") {
		t.Errorf("the conversation was logged: %s", got)
	}
}

func TestNilLogDiscardsRefusals(t *testing.T) {
	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	srv, _, token := setup(t, OpenAI, http.StatusBadRequest)
	header := withKey("Authorization", "Bearer "+token)

	wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/v1/models", header), http.StatusForbidden, "route")
	wantStatus(t, callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", `{"model":"allowed","future_field":true}`, header), http.StatusForbidden, "request")
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/responses", header), http.StatusBadRequest, "provider")

	if got := logs.String(); got != "" {
		t.Errorf("a refusal reached the default logger: %s", got)
	}
}

func TestShapeListsEachItemShapeOnceInOrder(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`[{"type":"b"},{"type":"a"},{"type":"b"},1,{"type":"a"}]`), &v); err != nil {
		t.Fatal(err)
	}

	if got, want := shape(v), "[{type=b} {type=a} float64]"; got != want {
		t.Errorf("shape = %q, want %q", got, want)
	}
}

func TestProviderRefusalsAreLoggedWithoutTheirBody(t *testing.T) {
	logs, opts := captureLogs()
	srv, _, token := setupWith(t, OpenAI, http.StatusBadRequest, opts)

	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/v1/responses", withKey("Authorization", "Bearer "+token)), http.StatusBadRequest, "provider refusal")
	got := logs.String()
	if !strings.Contains(got, `msg="model provider refused a request" method=POST route=/v1/responses status=400`) {
		t.Errorf("logs = %s, want the provider refusal", got)
	}
	if strings.Contains(got, `"ok"`) || strings.Contains(got, `\"ok\"`) {
		t.Errorf("the provider's body was logged: %s", got)
	}
}

func FuzzProviderRequest(f *testing.F) {
	for _, seed := range []string{`{"model":"allowed"}`, `{"model":"allowed","tools":[{"type":"function","name":"local","parameters":{}}]}`, `{"model":"allowed","input":[{"type":"additional_tools","tools":[{"type":"shell"}]}]}`, `{"model":"allowed","tools":[{"type":{}}]}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		for _, provider := range []Provider{Anthropic, OpenAI, ChatGPT} {
			proxy, err := New(provider, "fixture", "allowed", Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = proxy.safeBody([]byte(body))
		}
	})
}

func TestChatGPTServesTheModelCatalogue(t *testing.T) {
	srv, calls, token := setup(t, ChatGPT, http.StatusOK)

	resp := call(t, srv, http.MethodGet, PathPrefix+"/backend-api/codex/models?client_version=0.163.0", withKey("Authorization", "Bearer "+token))

	wantStatus(t, resp, http.StatusOK, "models")
	if len(*calls) != 1 || (*calls)[0].path != "/backend-api/codex/models" {
		t.Errorf("upstream calls = %+v", *calls)
	}
}

func TestExpectedRefusalsAreNotLogged(t *testing.T) {
	logs, opts := captureLogs()
	srv, _, token := setupWith(t, ChatGPT, http.StatusOK, opts)
	header := withKey("Authorization", "Bearer "+token)

	wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/backend-api/wham/accounts/check", header), http.StatusForbidden, "accounts check")
	wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/backend-api/wham/settings/user", header), http.StatusForbidden, "user settings")
	if got := logs.String(); got != "" {
		t.Fatalf("an expected refusal was logged: %s", got)
	}

	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/backend-api/wham/accounts/check", header), http.StatusForbidden, "accounts check by POST")
	if got := logs.String(); !strings.Contains(got, `msg="model proxy refused a route" method=POST route=/backend-api/wham/accounts/check`) {
		t.Errorf("logs = %s, want the unexpected refusal", got)
	}
}

func TestExpectedRefusalsDoNotSpendTheQuota(t *testing.T) {
	srv, calls, token := setup(t, ChatGPT, http.StatusOK)
	header := withKey("Authorization", "Bearer "+token)
	for range defaultMaxRequests + 1 {
		wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/backend-api/wham/accounts/check", header), http.StatusForbidden, "accounts check")
	}
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/backend-api/codex/responses", header), http.StatusOK, "expected refusals spent the quota")

	for range defaultMaxRequests {
		call(t, srv, http.MethodGet, PathPrefix+"/backend-api/ps/mcp", header)
	}
	wantStatus(t, call(t, srv, http.MethodPost, PathPrefix+"/backend-api/codex/responses", header), http.StatusUnauthorized, "unexpected refusals did not spend the quota")
	if len(*calls) != 1 {
		t.Errorf("upstream calls = %d, want 1", len(*calls))
	}
}

func TestOpenAIDropsTheServiceTier(t *testing.T) {
	srv, calls, token := setup(t, OpenAI, http.StatusOK)

	resp := callBody(t, srv, http.MethodPost, PathPrefix+"/v1/responses", `{"model":"allowed","service_tier":"priority"}`, withKey("Authorization", "Bearer "+token))

	wantStatus(t, resp, http.StatusOK, "responses")
	if len(*calls) != 1 || strings.Contains((*calls)[0].body, "service_tier") {
		t.Errorf("the service tier reached the upstream: %+v", *calls)
	}
}

func TestLongestUpstreamPrefixWins(t *testing.T) {
	p := Provider{
		Upstreams: map[string]string{"": "https://root.example/root", "/auth": "https://auth.example/accounts"},
		Routes:    func(string, string) bool { return true },
		SetKey:    func(http.Header, string) {},
	}
	srv, calls, token := setup(t, p, http.StatusOK)

	for range 20 {
		wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/auth/whoami", withKey("X-Api-Key", token)), http.StatusOK, "auth route")
		wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/authz", withKey("X-Api-Key", token)), http.StatusOK, "root route")
	}

	for i, got := range *calls {
		want := "/accounts/whoami"
		if i%2 == 1 {
			want = "/root/authz"
		}
		if got.path != want {
			t.Fatalf("call %d reached %q, want %q", i, got.path, want)
		}
	}
}

func TestRouteNoUpstreamServesIsRefused(t *testing.T) {
	p := Provider{
		Upstreams: map[string]string{"/auth": "https://auth.example/accounts"},
		Routes:    func(string, string) bool { return true },
		SetKey:    func(http.Header, string) {},
	}
	srv, calls, token := setup(t, p, http.StatusOK)

	wantStatus(t, call(t, srv, http.MethodGet, PathPrefix+"/backend-api/codex/models", withKey("X-Api-Key", token)), http.StatusForbidden, "unserved route")
	if len(*calls) != 0 {
		t.Errorf("an unserved route reached an upstream: %+v", *calls)
	}
}
