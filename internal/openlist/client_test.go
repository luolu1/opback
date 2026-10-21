package openlist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func writeTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestRetriesRejectedTokenInSuccessEnvelope(t *testing.T) {
	var logins atomic.Int32
	var mkdirs atomic.Int32
	var mu sync.Mutex
	var auths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			n := logins.Add(1)
			var creds struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
				t.Errorf("decode login: %v", err)
			}
			if creds.Username != "user" || creds.Password != "pass" {
				t.Errorf("login credentials = %q/%q", creds.Username, creds.Password)
			}
			writeTestJSON(t, w, map[string]any{
				"code":    200,
				"message": "success",
				"data":    map[string]string{"token": fmt.Sprintf("token-%d", n)},
			})
		case "/api/fs/mkdir":
			mkdirs.Add(1)
			token := r.Header.Get("Authorization")
			mu.Lock()
			auths = append(auths, token)
			mu.Unlock()
			if token == "token-1" {
				writeTestJSON(t, w, map[string]any{
					"code":    401,
					"message": "token is expired",
					"data":    nil,
				})
				return
			}
			writeTestJSON(t, w, map[string]any{
				"code":    200,
				"message": "success",
				"data":    nil,
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "user", "pass")
	if err := c.Mkdir(context.Background(), "/backups"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("login count = %d, want 2", got)
	}
	if got := mkdirs.Load(); got != 2 {
		t.Fatalf("mkdir count = %d, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 2 || auths[0] != "token-1" || auths[1] != "token-2" {
		t.Fatalf("authorization headers = %v, want [token-1 token-2]", auths)
	}
}

func TestPutReplaysBodyAfterTokenRefresh(t *testing.T) {
	var logins atomic.Int32
	var mu sync.Mutex
	var bodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			n := logins.Add(1)
			writeTestJSON(t, w, map[string]any{
				"code":    200,
				"message": "success",
				"data":    map[string]string{"token": fmt.Sprintf("token-%d", n)},
			})
		case "/api/fs/put":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
			}
			token := r.Header.Get("Authorization")
			mu.Lock()
			bodies = append(bodies, string(body))
			mu.Unlock()
			if token == "token-1" {
				writeTestJSON(t, w, map[string]any{
					"code":    401,
					"message": "token is expired",
					"data":    nil,
				})
				return
			}
			writeTestJSON(t, w, map[string]any{
				"code":    200,
				"message": "success",
				"data":    nil,
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	const payload = "hello world"
	c := New(srv.URL, "user", "pass")
	err := c.Put(context.Background(), "/backups/file.txt", strings.NewReader(payload), int64(len(payload)), PutOptions{Overwrite: true})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("login count = %d, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upload count = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		if body != payload {
			t.Fatalf("upload %d body = %q, want %q", i, body, payload)
		}
	}
}
