package skills

import (
	"strings"
	"testing"
)

func TestParseRepoSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		wantURL string
		wantRef string
	}{
		{
			raw:     "owner/repo",
			wantURL: "https://github.com/owner/repo",
		},
		{
			raw:     "owner/repo@v1.2.3",
			wantURL: "https://github.com/owner/repo",
			wantRef: "v1.2.3",
		},
		{
			raw:     "https://git.example.test/team/repo@main",
			wantURL: "https://git.example.test/team/repo",
			wantRef: "main",
		},
		{
			raw:     "https://git.example.test/team/repo",
			wantURL: "https://git.example.test/team/repo",
		},
	}
	for _, test := range tests {
		url, ref, err := ParseRepoSpec(test.raw)
		if err != nil {
			t.Errorf("ParseRepoSpec(%q): %v", test.raw, err)
			continue
		}
		if url != test.wantURL || ref != test.wantRef {
			t.Errorf("ParseRepoSpec(%q) = %q, %q", test.raw, url, ref)
		}
	}
	for _, raw := range []string{"", "owner", "ssh://git.example.test/repo"} {
		if _, _, err := ParseRepoSpec(raw); err == nil {
			t.Errorf("ParseRepoSpec(%q) succeeded", raw)
		}
	}

	for _, test := range []struct {
		raw    string
		secret string
	}{
		{"https://token@git.example.test/team/repo", "token"},
		{"https://user:supersecret@git.example.test/team/repo", "supersecret"},
		{"https://token%40tenant@git.example.test/team/repo", "token@tenant"},
		{"https://token@git.example.test/team/repo@main", "token"},
	} {
		url, ref, err := ParseRepoSpec(test.raw)
		if err == nil {
			t.Errorf("ParseRepoSpec(%q) succeeded", test.raw)
			continue
		}
		if url != "" || ref != "" {
			t.Errorf("ParseRepoSpec(%q) returned %q, %q on error", test.raw, url, ref)
		}
		if !strings.Contains(err.Error(), "userinfo") {
			t.Errorf("ParseRepoSpec(%q) error = %q, want userinfo error", test.raw, err)
		}
		if strings.Contains(err.Error(), test.raw) || strings.Contains(err.Error(), test.secret) {
			t.Errorf("ParseRepoSpec(%q) leaked credentials in error %q", test.raw, err)
		}
	}
}
