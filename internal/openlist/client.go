// Package openlist is a small, dependency-free client for the OpenList HTTP
// API (https://fox.oplist.org/). It implements exactly the subset needed by
// the backup agent and the panel's directory browser:
//
//   - POST /api/auth/login       authenticate and obtain a JWT
//   - POST /api/fs/list          list a directory
//   - POST /api/fs/get           stat a single file/dir
//   - POST /api/fs/mkdir         create a directory
//   - PUT  /api/fs/put           stream-upload a file
//   - POST /api/fs/move          move/rename files or directories
//   - POST /api/fs/remove        delete files or directories
//   - POST /api/fs/remove_empty_directory  recursively remove empty dirs
//
// Authentication uses the raw token in the Authorization header (no "Bearer"
// prefix), matching OpenList's documented scheme. A rejected token, whether
// returned as HTTP 401 or as an OpenList envelope code 401, triggers a single
// re-login attempt.
package openlist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FsObject mirrors a single entry returned by /api/fs/list and /api/fs/get.
type FsObject struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	IsDir    bool   `json:"is_dir"`
	Modified string `json:"modified"`
	Created  string `json:"created"`
	Sign     string `json:"sign"`
	Type     int    `json:"type"`
}

// ListResult is the payload of a successful /api/fs/list call.
type ListResult struct {
	Content  []FsObject `json:"content"`
	Total    int        `json:"total"`
	Write    bool       `json:"write"`
	Provider string     `json:"provider"`
}

// PutOptions controls a single upload.
type PutOptions struct {
	// Overwrite replaces an existing destination file. Defaults to true when
	// the header is omitted by the server, but we set it explicitly.
	Overwrite bool
	// LastModified sets the destination modification time.
	LastModified time.Time
	// MD5 / SHA1 / SHA256, when set, travel ahead of the data so supporting
	// drivers can finish without receiving the body.
	MD5    string
	SHA1   string
	SHA256 string
}

// Client talks to a single OpenList instance.
type Client struct {
	baseURL  string
	username string
	password string

	http *http.Client

	mu    sync.Mutex
	token string
}

// New builds a client. baseURL may or may not include a trailing slash.
func New(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http: &http.Client{
			Timeout: 0, // uploads of large files must not time out
		},
	}
}

// apiResponse is the common envelope used by every OpenList endpoint.
type apiResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Login authenticates and caches the resulting token.
func (c *Client) Login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"username": c.username,
		"password": c.password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openlist: login request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env apiResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("openlist: login: bad response (http %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode != http.StatusOK || env.Code != http.StatusOK {
		return fmt.Errorf("openlist: login failed: %s", firstNonEmpty(env.Message, resp.Status))
	}

	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.Token == "" {
		return fmt.Errorf("openlist: login: missing token in response")
	}

	c.mu.Lock()
	c.token = data.Token
	c.mu.Unlock()
	return nil
}

// SetToken injects a token directly, skipping the login round trip.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

func (c *Client) ensureToken(ctx context.Context) error {
	if c.currentToken() != "" {
		return nil
	}
	return c.Login(ctx)
}

// bodyFactory produces a fresh request body for each attempt so a request can
// be replayed after a re-login.
type bodyFactory func() (io.ReadCloser, int64, error)

func bytesBody(b []byte) bodyFactory {
	return func() (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	}
}

// do performs a request, ensuring a token and retrying once when the token is
// rejected. OpenList reports an expired token either as HTTP 401 or, for some
// endpoints, as HTTP 200 with {"code":401,...}; both must trigger a re-login.
func (c *Client) do(ctx context.Context, method, apiPath string, header http.Header, makeBody bodyFactory) (*apiResponse, error) {
	if err := c.ensureToken(ctx); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		var body io.ReadCloser
		var size int64
		if makeBody != nil {
			var err error
			body, size, err = makeBody()
			if err != nil {
				return nil, err
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+apiPath, body)
		if err != nil {
			if body != nil {
				body.Close()
			}
			return nil, err
		}
		req.Header.Set("Authorization", c.currentToken())
		if makeBody != nil {
			req.ContentLength = size
		}
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Set(k, v)
			}
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()

		env := &apiResponse{}
		if len(raw) > 0 {
			// Not every endpoint guarantees JSON; fall back to status text.
			if err := json.Unmarshal(raw, env); err != nil {
				env.Code = resp.StatusCode
				env.Message = strings.TrimSpace(string(raw))
			}
		} else {
			env.Code = resp.StatusCode
		}

		authFailure := resp.StatusCode == http.StatusUnauthorized || env.Code == http.StatusUnauthorized
		if authFailure && attempt == 0 {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			if err := c.Login(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if authFailure {
			if env.Message == "" {
				env.Message = "unauthorized"
			}
			return env, fmt.Errorf("openlist: %s %s: %s", method, apiPath, env.Message)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if env.Message == "" {
				env.Message = resp.Status
			}
			return env, fmt.Errorf("openlist: %s %s: %s", method, apiPath, env.Message)
		}
		if env.Code != 0 && env.Code != http.StatusOK {
			return env, fmt.Errorf("openlist: %s %s: %s", method, apiPath, firstNonEmpty(env.Message, strconv.Itoa(env.Code)))
		}
		return env, nil
	}
	return nil, fmt.Errorf("openlist: %s %s: request failed", method, apiPath)
}

// List returns the contents of an OpenList directory. Because the API caps
// per_page at 100, it paginates until every entry has been collected.
func (c *Client) List(ctx context.Context, dir string, refresh bool) (*ListResult, error) {
	const perPage = 100
	const maxPages = 1000

	var all []FsObject
	var first ListResult
	for page := 1; page <= maxPages; page++ {
		payload, _ := json.Marshal(map[string]any{
			"path":     normalize(dir),
			"password": "",
			"refresh":  refresh && page == 1,
			"page":     page,
			"per_page": perPage,
		})
		h := http.Header{"Content-Type": []string{"application/json"}}
		env, err := c.do(ctx, http.MethodPost, "/api/fs/list", h, bytesBody(payload))
		if err != nil {
			return nil, err
		}
		var res ListResult
		if len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, &res); err != nil {
				return nil, err
			}
		}
		if page == 1 {
			first = res
		}
		all = append(all, res.Content...)
		if len(res.Content) < perPage || len(all) >= res.Total {
			break
		}
	}
	first.Content = all
	first.Total = len(all)
	return &first, nil
}

// Get stats a single path.
func (c *Client) Get(ctx context.Context, p string) (*FsObject, error) {
	payload, _ := json.Marshal(map[string]any{"path": normalize(p), "password": ""})
	h := http.Header{"Content-Type": []string{"application/json"}}
	env, err := c.do(ctx, http.MethodPost, "/api/fs/get", h, bytesBody(payload))
	if err != nil {
		return nil, err
	}
	obj := &FsObject{}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, obj); err != nil {
			return nil, err
		}
	}
	return obj, nil
}

// Mkdir creates a single directory. It is not an error if the directory
// already exists.
func (c *Client) Mkdir(ctx context.Context, dir string) error {
	payload, _ := json.Marshal(map[string]any{"path": normalize(dir)})
	h := http.Header{"Content-Type": []string{"application/json"}}
	_, err := c.do(ctx, http.MethodPost, "/api/fs/mkdir", h, bytesBody(payload))
	if err != nil && isExistsErr(err) {
		return nil
	}
	return err
}

// EnsureDir creates every missing component of dir, like `mkdir -p`.
func (c *Client) EnsureDir(ctx context.Context, dir string) error {
	dir = normalize(dir)
	if dir == "/" {
		return nil
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	cur := ""
	for _, p := range parts {
		cur += "/" + p
		if err := c.Mkdir(ctx, cur); err != nil {
			return err
		}
	}
	return nil
}

// Put streams a file to destPath. size is the number of bytes that will be
// read from r; pass -1 if unknown. r must be seekable so an upload can be
// replayed after the token is refreshed.
func (c *Client) Put(ctx context.Context, destPath string, r io.ReadSeeker, size int64, opts PutOptions) error {
	if size < 0 {
		return fmt.Errorf("openlist: put %s: unknown content length", destPath)
	}
	makeBody := func() (io.ReadCloser, int64, error) {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return nil, 0, fmt.Errorf("openlist: put %s: rewind: %w", destPath, err)
		}
		return io.NopCloser(r), size, nil
	}

	h := http.Header{}
	h.Set("File-Path", encodeFilePath(destPath))
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Overwrite", strconv.FormatBool(opts.Overwrite))
	if !opts.LastModified.IsZero() {
		h.Set("Last-Modified", strconv.FormatInt(opts.LastModified.UnixMilli(), 10))
	}
	if opts.MD5 != "" {
		h.Set("X-File-Md5", opts.MD5)
	}
	if opts.SHA1 != "" {
		h.Set("X-File-Sha1", opts.SHA1)
	}
	if opts.SHA256 != "" {
		h.Set("X-File-Sha256", opts.SHA256)
	}

	if _, err := c.do(ctx, http.MethodPut, "/api/fs/put", h, makeBody); err != nil {
		return err
	}
	return nil
}

// Remove deletes the named entries inside dir.
func (c *Client) Remove(ctx context.Context, dir string, names []string) error {
	payload, _ := json.Marshal(map[string]any{"dir": normalize(dir), "names": names})
	h := http.Header{"Content-Type": []string{"application/json"}}
	_, err := c.do(ctx, http.MethodPost, "/api/fs/remove", h, bytesBody(payload))
	return err
}

// Move relocates the named entries from srcDir into dstDir. dstDir must
// already exist.
func (c *Client) Move(ctx context.Context, srcDir, dstDir string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{
		"src_dir": normalize(srcDir),
		"dst_dir": normalize(dstDir),
		"names":   names,
	})
	h := http.Header{"Content-Type": []string{"application/json"}}
	_, err := c.do(ctx, http.MethodPost, "/api/fs/move", h, bytesBody(payload))
	return err
}

// RemoveEmptyDirs recursively deletes empty directories under dir.
func (c *Client) RemoveEmptyDirs(ctx context.Context, dir string) error {
	payload, _ := json.Marshal(map[string]any{"src_dir": normalize(dir)})
	h := http.Header{"Content-Type": []string{"application/json"}}
	_, err := c.do(ctx, http.MethodPost, "/api/fs/remove_empty_directory", h, bytesBody(payload))
	return err
}

// Test verifies the connection by logging in and listing the root directory.
func (c *Client) Test(ctx context.Context) error {
	if err := c.Login(ctx); err != nil {
		return err
	}
	_, err := c.List(ctx, "/", false)
	return err
}

// normalize cleans a path into the absolute slash form OpenList expects.
func normalize(p string) string {
	if p == "" {
		return "/"
	}
	// OpenList paths always use forward slashes even on Windows agents.
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// encodeFilePath percent-encodes each path segment, keeping the separators.
// Non-ASCII names must be encoded because the value travels in an HTTP header.
func encodeFilePath(p string) string {
	p = normalize(p)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(parts, "/")
}

func isExistsErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "exist") || strings.Contains(msg, "already")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
