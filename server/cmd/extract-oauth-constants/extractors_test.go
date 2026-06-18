package main

import (
	"strings"
	"testing"
)

func TestEndpointExtractor(t *testing.T) {
	cases := []struct {
		name    string
		hits    []StringHit
		want    string
		wantErr string
	}{
		{
			name: "happy path — exact URL embedded",
			hits: []StringHit{
				{Offset: 100, Value: "var ep=\"https://platform.claude.com/v1/oauth/token\";"},
			},
			want: "https://platform.claude.com/v1/oauth/token",
		},
		{
			name:    "missing endpoint URL",
			hits:    []StringHit{{Offset: 100, Value: "/v1/oauth/token"}},
			wantErr: "not present",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := endpointExtractor().Run(tc.hits)
			checkResult(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func TestVersionHeaderExtractor(t *testing.T) {
	cases := []struct {
		name    string
		hits    []StringHit
		want    string
		wantErr string
	}{
		{
			name: "happy path",
			hits: []StringHit{
				{Offset: 100, Value: "oauth-2025-04-20"},
				{Offset: 200, Value: "some other string"},
			},
			want: "oauth-2025-04-20",
		},
		{
			name:    "no header",
			hits:    []StringHit{{Offset: 100, Value: "other"}},
			wantErr: "no oauth-YYYY-MM-DD",
		},
		{
			name: "ambiguous",
			hits: []StringHit{
				{Offset: 100, Value: "oauth-2025-04-20"},
				{Offset: 200, Value: "oauth-2026-01-01"},
			},
			wantErr: "multiple candidates",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := versionHeaderExtractor().Run(tc.hits)
			checkResult(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func TestClientIDExtractor(t *testing.T) {
	const anchor = "platform.claude.com/oauth/code/callback"
	const goodUUID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	const designUUID = "59637612-477b-4836-a601-b0589eda7704"
	const localUUID = "22422756-60c9-4084-8eb7-27705fd5cf9a"
	const otherUUID = "00000000-1111-2222-3333-444444444444"

	cases := []struct {
		name    string
		hits    []StringHit
		want    string
		wantErr string
	}{
		{
			// claude >= 2.1.181 emits DESIGN_CLIENT_ID next to CLIENT_ID; the
			// whole-token match must take CLIENT_ID and skip the sibling.
			name: "happy path with sibling DESIGN_CLIENT_ID",
			hits: []StringHit{
				{Offset: 1000, Value: "...some other string..."},
				{Offset: 1500, Value: `MANUAL_REDIRECT_URL:"https://` + anchor +
					`",CLIENT_ID:"` + goodUUID + `",DESIGN_CLIENT_ID:"` + designUUID + `"`},
			},
			want: goodUUID,
		},
		{
			// Several config blocks share one string run in the real binary.
			// The local block lacks the literal anchor and its CLIENT_ID sits
			// outside the window, so only the production block is selected.
			name: "production block selected among multiple blocks",
			hits: []StringHit{
				{Offset: 0, Value: "MANUAL_REDIRECT_URL:`${n}/oauth/code/callback`,CLIENT_ID:\"" +
					localUUID + "\"," + strings.Repeat("x", 600) +
					`MANUAL_REDIRECT_URL:"https://` + anchor + `",CLIENT_ID:"` + goodUUID + `"`},
			},
			want: goodUUID,
		},
		{
			name: "client_id field too far from anchor",
			hits: []StringHit{
				{Offset: 1500, Value: anchor},
				{Offset: 100000, Value: `CLIENT_ID:"` + goodUUID + `"`},
			},
			wantErr: "no CLIENT_ID field within",
		},
		{
			name:    "anchor missing",
			hits:    []StringHit{{Offset: 1700, Value: `CLIENT_ID:"` + goodUUID + `"`}},
			wantErr: "anchor",
		},
		{
			name: "ambiguous",
			hits: []StringHit{
				{Offset: 1500, Value: anchor},
				{Offset: 1600, Value: `CLIENT_ID:"` + goodUUID + `"`},
				{Offset: 1700, Value: `CLIENT_ID:"` + otherUUID + `"`},
			},
			wantErr: "ambiguous",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clientIDExtractor().Run(tc.hits)
			checkResult(t, got, err, tc.want, tc.wantErr)
		})
	}
}

func TestScopesExtractor(t *testing.T) {
	all := []StringHit{
		{Offset: 100, Value: "user:profile user:inference user:sessions:claude_code user:mcp_servers"},
	}
	got, err := scopesExtractor().Run(all)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if got != "user:profile user:inference user:sessions:claude_code user:mcp_servers" {
		t.Errorf("got %q", got)
	}

	missing := []StringHit{
		{Offset: 100, Value: "user:profile user:inference user:sessions:claude_code"},
	}
	_, err = scopesExtractor().Run(missing)
	if err == nil || !strings.Contains(err.Error(), "user:mcp_servers") {
		t.Errorf("missing-scope error = %v", err)
	}
}

func TestRun_MultiFailureReporting(t *testing.T) {
	// Hits missing both /v1/oauth/token and the version header.
	hits := []StringHit{
		{Offset: 100, Value: "api.anthropic.com"},
		{Offset: 200, Value: "user:profile user:inference user:sessions:claude_code user:mcp_servers"},
		{Offset: 1500, Value: `MANUAL_REDIRECT_URL:"https://platform.claude.com/oauth/code/callback",CLIENT_ID:"9d1c250a-e61b-44d9-88ed-5944d1962f5e"`},
	}
	_, errs := Run(hits)
	if len(errs) < 2 {
		t.Fatalf("expected >= 2 errors (endpoint + version_header), got %d: %v", len(errs), errs)
	}
	var sawEndpoint, sawVersion bool
	for _, e := range errs {
		s := e.Error()
		if strings.HasPrefix(s, "endpoint:") {
			sawEndpoint = true
		}
		if strings.HasPrefix(s, "version_header:") {
			sawVersion = true
		}
	}
	if !sawEndpoint || !sawVersion {
		t.Errorf("expected both endpoint + version_header failures: %v", errs)
	}
}

func checkResult(t *testing.T, got string, err error, want, wantErr string) {
	t.Helper()
	if wantErr != "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("err = %v, want substring %q", err, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
