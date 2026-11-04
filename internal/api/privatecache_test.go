package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// A shared GET cache with a positive heuristic TTL may store a 200 unless the
// response forbids it. It must never bypass auth for the next caller at this URL.
func TestPrivateResponsesForbidSharedCache(t *testing.T) {
	s := newPrivateServer(t)
	developer := seedMember(t, s, "cache-developer", "developer", "console")
	user := seedMember(t, s, "cache-user", "user", "console")
	cookie, _ := s.issue(developer)
	userCookie, _ := s.issue(user)
	id := domain.NewID(domain.PrefixFile).String()
	const content = "\x00\xffprivate file\r\n"
	if err := s.blobs.Put(context.Background(), "files/"+id, strings.NewReader(content), int64(len(content)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO files(id,filename,mime_type,size_bytes,downloadable) VALUES ($1,'private.bin','application/octet-stream',$2,true)`, id, len(content)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{privateProxy + "/agents", privateProxy + "/files/" + id + "/content"} {
		t.Run(path, func(t *testing.T) {
			res, raw := s.request("GET", path, "", nativeHeaders(cookie, ""))
			if res.StatusCode != 200 {
				t.Fatalf("developer response=%d %s", res.StatusCode, raw)
			}
			if strings.HasSuffix(path, "/content") && (string(raw) != content || res.Header.Get("Content-Disposition") != `attachment; filename="private.bin"`) {
				t.Fatal("file bytes/filename changed")
			}
			policy := strings.ToLower(strings.Join(res.Header.Values("Cache-Control"), ","))
			// RFC 9111: private or no-store forbids storage by a shared cache. Require
			// both here, so private browser storage is also explicitly disabled.
			stored := !strings.Contains(policy, "private") && !strings.Contains(policy, "no-store")
			if !strings.Contains(policy, "private") || !strings.Contains(policy, "no-store") {
				t.Errorf("private response policy=%q", policy)
			}
			for _, caller := range []struct {
				name, cookie string
				want         int
			}{{"anonymous", "", 401}, {"user", userCookie, 403}, {"revoked", cookie, 401}} {
				if caller.name == "revoked" {
					if _, err := s.pool.Exec(context.Background(), `DELETE FROM browser_sessions WHERE principal_id=$1`, developer); err != nil {
						t.Fatal(err)
					}
				}
				status := 200
				if !stored {
					next, _ := s.request("GET", path, "", nativeHeaders(caller.cookie, ""))
					status = next.StatusCode
				}
				if status != caller.want {
					t.Errorf("heuristic shared cache served developer response to %s: status=%d want %d", caller.name, status, caller.want)
				}
			}
			cookie, _ = s.issue(developer)
		})
	}
	// The public machine-key surface keeps its existing response header contract.
	res, _ := s.request("GET", "/v1/agents", "", http.Header{"X-Api-Key": {testKey}})
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "" {
		t.Fatalf("public SDK policy changed: %d %v", res.StatusCode, res.Header)
	}
}
