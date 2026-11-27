package workspace

import (
	"net/http"
	"strings"
	"unicode/utf8"
)

// PrivateSelector reads the current tab's workspace. A browser session itself is
// organization-wide. The path and selector must agree when both are present.
func PrivateSelector(r *http.Request) (string, bool) {
	values := r.Header.Values("x-workspace-id")
	var value string
	if len(values) > 0 {
		if len(values) != 1 || values[0] == "" {
			return "", false
		}
		value = values[0]
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) >= 7 && parts[1] == "api" && parts[2] == "console" && parts[3] == "organizations" && parts[5] == "workspaces" {
		if value != "" && value != parts[6] {
			return "", false
		}
		value = parts[6]
	}
	return value, len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsAny(value, "/|\x00")
}
