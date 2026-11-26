// Package amazon talks to the undocumented Amazon Photos web API using the
// cookies of a logged-in browser session.
package amazon

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// tldBySuffix maps the country suffix of Amazon's auth cookies (at-acbXX) to
// the site's top-level domain, where the two differ.
var tldBySuffix = map[string]string{
	"uk": "co.uk",
	"jp": "co.jp",
	"au": "com.au",
	"br": "com.br",
	"mx": "com.mx",
}

// ParseCookies accepts what a person can realistically paste: the raw value of
// a Cookie request header (with or without the "Cookie:" prefix), a JSON
// object {name: value}, or the JSON array produced by cookie-export extensions.
func ParseCookies(input string) (map[string]string, error) {
	s := strings.TrimSpace(input)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] && !strings.HasPrefix(s, `"{`) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if len(s) >= 7 && strings.EqualFold(s[:7], "cookie:") {
		s = strings.TrimSpace(s[7:])
	}
	out := map[string]string{}
	switch {
	case strings.HasPrefix(s, "{"):
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, fmt.Errorf("invalid JSON cookie object: %w", err)
		}
	case strings.HasPrefix(s, "["):
		var list []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal([]byte(s), &list); err != nil {
			return nil, fmt.Errorf("invalid JSON cookie array: %w", err)
		}
		for _, c := range list {
			if c.Name != "" {
				out[c.Name] = c.Value
			}
		}
	default:
		for _, part := range strings.Split(s, ";") {
			name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			name = strings.TrimSpace(name)
			if !ok || name == "" || strings.ContainsAny(name, " \t\r\n") {
				continue
			}
			out[name] = strings.TrimSpace(value)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no cookies found in input")
	}
	return out, nil
}

// DetectTLD derives the Amazon site (fr, de, co.uk, com, ...) from the name of
// the auth cookie.
func DetectTLD(cookies map[string]string) (string, bool) {
	suffix, ok := siteSuffix(cookies)
	if !ok {
		return "", false
	}
	if suffix == "main" {
		return "com", true
	}
	if tld, ok := tldBySuffix[suffix]; ok {
		return tld, true
	}
	return suffix, true
}

// siteSuffix returns "main" for amazon.com cookies, or the country suffix XX
// of at-acbXX / ubid-acbXX.
func siteSuffix(cookies map[string]string) (string, bool) {
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, prefix := range []string{"at-acb", "ubid-acb"} {
		for _, name := range names {
			if rest, ok := strings.CutPrefix(name, prefix); ok && rest != "" {
				return rest, true
			}
		}
	}
	for _, name := range names {
		if name == "at_main" || name == "ubid_main" {
			return "main", true
		}
	}
	return "", false
}

// MissingCookies lists the cookies the API needs that are absent from the set.
func MissingCookies(cookies map[string]string) []string {
	required := []string{"session-id"}
	switch suffix, ok := siteSuffix(cookies); {
	case !ok:
		required = append(required, "ubid-acb<xx>", "at-acb<xx>")
	case suffix == "main":
		required = append(required, "ubid_main", "at_main")
	default:
		required = append(required, "ubid-acb"+suffix, "at-acb"+suffix)
	}
	var missing []string
	for _, name := range required {
		if cookies[name] == "" {
			missing = append(missing, name)
		}
	}
	return missing
}
