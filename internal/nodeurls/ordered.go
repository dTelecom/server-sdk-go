// Package nodeurls preserves the issuer's node preference for room connections.
package nodeurls

import (
	"net/url"
	"strings"
)

// Ordered puts preferred first and skips repeated or invalid endpoints.
// A secure primary never falls back to a plaintext connection.
func Ordered(preferred string, urls []string) []string {
	result := make([]string, 0, len(urls)+1)
	seen := make(map[string]bool, len(urls)+1)
	secure := false
	add := func(raw string) {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return
		}
		switch u.Scheme {
		case "http", "ws":
			u.Scheme = "ws"
		case "https", "wss":
			u.Scheme = "wss"
		default:
			return
		}
		u.Host = strings.ToLower(u.Host)
		if (u.Scheme == "ws" && u.Port() == "80") || (u.Scheme == "wss" && u.Port() == "443") {
			u.Host = u.Host[:strings.LastIndexByte(u.Host, ':')]
		}
		key := u.String()
		if seen[key] || (secure && u.Scheme == "ws") {
			return
		}
		if len(result) == 0 {
			secure = u.Scheme == "wss"
		}
		seen[key] = true
		result = append(result, raw)
	}
	add(preferred)
	for _, node := range urls {
		add(node)
	}
	return result
}
