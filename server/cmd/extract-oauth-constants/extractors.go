package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ExtractedConstants is the result of running every extractor over a binary.
type ExtractedConstants struct {
	Endpoint      string `json:"endpoint"`
	ClientID      string `json:"client_id"`
	VersionHeader string `json:"version_header"`
	Scopes        string `json:"scopes"`
}

// Extractor describes a single constant we want to pull out of the binary.
// Each extractor is self-contained: it owns its anchors, regex, and pass/fail
// logic. Adding a new constant means appending one Extractor — no integration
// with the rest of the program.
type Extractor struct {
	Name string
	Doc  string
	Run  func(hits []StringHit) (string, error)
}

func All() []Extractor {
	return []Extractor{
		endpointExtractor(),
		versionHeaderExtractor(),
		clientIDExtractor(),
		scopesExtractor(),
	}
}

// Run runs every extractor and returns the populated struct plus the
// concatenated list of failures. The caller exits non-zero if errs is non-empty.
// Multi-failure reporting matters: when Anthropic rotates one constant, we
// want to see ALL changes in one extraction pass rather than bisecting.
func Run(hits []StringHit) (ExtractedConstants, []error) {
	out := ExtractedConstants{}
	var errs []error
	for _, e := range All() {
		v, err := e.Run(hits)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name, err))
			continue
		}
		switch e.Name {
		case "endpoint":
			out.Endpoint = v
		case "version_header":
			out.VersionHeader = v
		case "client_id":
			out.ClientID = v
		case "scopes":
			out.Scopes = v
		}
	}
	return out, errs
}

func endpointExtractor() Extractor {
	// Claude Code's production OAuth endpoint lives on platform.claude.com,
	// NOT api.anthropic.com. The plan's original research conflated the host
	// of /v1/messages with the host of /v1/oauth/token. The full URL appears
	// verbatim in the bundled JS, so we just require it.
	const wantURL = "https://platform.claude.com/v1/oauth/token"
	return Extractor{
		Name: "endpoint",
		Doc:  "OAuth token endpoint URL — the literal string must appear in the binary.",
		Run: func(hits []StringHit) (string, error) {
			for _, h := range hits {
				if strings.Contains(h.Value, wantURL) {
					return wantURL, nil
				}
			}
			return "", fmt.Errorf("%q not present — endpoint host or path may have changed", wantURL)
		},
	}
}

func versionHeaderExtractor() Extractor {
	re := regexp.MustCompile(`^oauth-20\d{2}-\d{2}-\d{2}$`)
	return Extractor{
		Name: "version_header",
		Doc:  "anthropic-version header value matching ^oauth-YYYY-MM-DD$",
		Run: func(hits []StringHit) (string, error) {
			set := map[string]struct{}{}
			for _, h := range hits {
				if re.MatchString(h.Value) {
					set[h.Value] = struct{}{}
				}
			}
			keys := sortedKeys(set)
			switch len(keys) {
			case 0:
				return "", fmt.Errorf("no oauth-YYYY-MM-DD header found")
			case 1:
				return keys[0], nil
			default:
				return "", fmt.Errorf("multiple candidates: %v", keys)
			}
		},
	}
}

func clientIDExtractor() Extractor {
	// The bundled JS carries several OAuth config objects (local, staging,
	// production). Only the production object embeds its URLs as string
	// literals — the others build them from template variables — so the literal
	// callback URL uniquely marks the production block. Within that block the
	// client id is the value of the CLIENT_ID field.
	//
	// Two field-level hazards drive the rest of the logic:
	//   - claude 2.1.181 added a sibling DESIGN_CLIENT_ID field right next to
	//     CLIENT_ID, so we match CLIENT_ID as a whole token. RE2 has no
	//     lookbehind, hence the consuming non-word prefix that rejects any
	//     *_CLIENT_ID (DESIGN_CLIENT_ID, etc).
	//   - every block has its own CLIENT_ID, so we keep only the field within a
	//     small window of the production anchor; the other blocks sit thousands
	//     of bytes away.
	const anchor = "platform.claude.com/oauth/code/callback"
	const window = 512
	clientIDRE := regexp.MustCompile(`(?:^|[^0-9A-Za-z_])CLIENT_ID:"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})"`)
	return Extractor{
		Name: "client_id",
		Doc:  fmt.Sprintf("Value of the CLIENT_ID field within %d bytes of the production OAuth callback URL.", window),
		Run: func(hits []StringHit) (string, error) {
			var anchors []int64
			for _, h := range hits {
				for i := 0; ; {
					idx := strings.Index(h.Value[i:], anchor)
					if idx < 0 {
						break
					}
					anchors = append(anchors, h.Offset+int64(i+idx))
					i += idx + 1
				}
			}
			if len(anchors) == 0 {
				return "", fmt.Errorf("anchor %q not found", anchor)
			}
			set := map[string]struct{}{}
			for _, h := range hits {
				for _, m := range clientIDRE.FindAllStringSubmatchIndex(h.Value, -1) {
					off := h.Offset + int64(m[2])
					for _, a := range anchors {
						if abs64(off-a) <= window {
							set[h.Value[m[2]:m[3]]] = struct{}{}
							break
						}
					}
				}
			}
			keys := sortedKeys(set)
			switch len(keys) {
			case 0:
				return "", fmt.Errorf("no CLIENT_ID field within %d bytes of anchor %q", window, anchor)
			case 1:
				return keys[0], nil
			default:
				return "", fmt.Errorf("ambiguous client_ids near anchor: %v — extractor needs an update", keys)
			}
		},
	}
}

func scopesExtractor() Extractor {
	wanted := []string{
		"user:profile",
		"user:inference",
		"user:sessions:claude_code",
		"user:mcp_servers",
	}
	return Extractor{
		Name: "scopes",
		Doc:  "Each of the four default Claude Code OAuth scopes must appear verbatim in the binary.",
		Run: func(hits []StringHit) (string, error) {
			var missing []string
			for _, s := range wanted {
				if !hasSubstring(hits, s) {
					missing = append(missing, s)
				}
			}
			if len(missing) > 0 {
				return "", fmt.Errorf("missing scope strings: %v — claude's scope set may have changed", missing)
			}
			return strings.Join(wanted, " "), nil
		},
	}
}

func hasSubstring(hits []StringHit, s string) bool {
	for _, h := range hits {
		if strings.Contains(h.Value, s) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
