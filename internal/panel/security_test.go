package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"opback/internal/store"
)

func newTestPanel(t *testing.T) (*store.Store, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st, New(st)
}

func newJarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// doJSON issues a JSON request and asserts the status code.
func doJSON(t *testing.T, c *http.Client, method, url string, body any, want int, out any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, url, resp.StatusCode, want, b)
	}
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
}

func TestTOTPVerifyAndSkew(t *testing.T) {
	secret := newTOTPSecret()
	if secret == "" {
		t.Fatal("no secret generated")
	}
	now := time.Now()
	if code := totpAt(secret, now); !verifyTOTP(secret, code, now) {
		t.Fatalf("current code %q rejected", code)
	}
	// One step of clock skew either way must still be accepted.
	if code := totpAt(secret, now.Add(-totpPeriod*time.Second)); !verifyTOTP(secret, code, now) {
		t.Fatal("code from previous step rejected")
	}
	if code := totpAt(secret, now.Add(totpPeriod*time.Second)); !verifyTOTP(secret, code, now) {
		t.Fatal("code from next step rejected")
	}
	// Two steps away is outside the window.
	if code := totpAt(secret, now.Add(3*totpPeriod*time.Second)); verifyTOTP(secret, code, now) {
		t.Fatal("code far outside the window accepted")
	}
	if verifyTOTP(secret, "000000", now) && totpAt(secret, now) != "000000" {
		t.Fatal("bogus code accepted")
	}
}

func TestSessionEpochInvalidatesSessions(t *testing.T) {
	st, srv := newTestPanel(t)
	tok := srv.signSession("admin", time.Now().Add(time.Hour))
	if !srv.verifySession(tok) {
		t.Fatal("fresh session rejected")
	}
	if err := st.BumpSessionEpoch(); err != nil {
		t.Fatal(err)
	}
	if srv.verifySession(tok) {
		t.Fatal("stale session accepted after epoch bump")
	}
}

func TestChallengeIsNotASession(t *testing.T) {
	_, srv := newTestPanel(t)
	ch := srv.signChallenge(time.Now().Add(5 * time.Minute))
	if !srv.verifyChallenge(ch) {
		t.Fatal("challenge rejected")
	}
	if srv.verifySession(ch) {
		t.Fatal("challenge accepted as a session")
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, srv := newTestPanel(t)
	ts := httptest.NewServer(srv.WebHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}

func TestOriginGuardBlocksCrossSite(t *testing.T) {
	_, srv := newTestPanel(t)
	ts := httptest.NewServer(srv.WebHandler())
	defer ts.Close()

	// A cross-site POST is rejected outright.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST status=%d, want 403", resp.StatusCode)
	}

	// A same-origin POST is allowed.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("same-origin POST status=%d, want 200", resp2.StatusCode)
	}

	// A mismatching Origin header is also rejected.
	req3, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	req3.Header.Set("Origin", "https://evil.example.com")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign-origin POST status=%d, want 403", resp3.StatusCode)
	}
}

func TestRateLimiterBlocksAndResets(t *testing.T) {
	rl := newRateLimiter(3, time.Minute, time.Minute)
	key := "10.0.0.1"
	for i := 0; i < 3; i++ {
		if ok, _ := rl.allow(key); !ok {
			t.Fatalf("attempt %d blocked too early", i)
		}
		rl.fail(key)
	}
	if ok, _ := rl.allow(key); ok {
		t.Fatal("expected the key to be blocked")
	}
	rl.reset(key)
	if ok, _ := rl.allow(key); !ok {
		t.Fatal("reset should clear the block")
	}
}

func TestAgentRegisterCannotHijackWithoutToken(t *testing.T) {
	st, srv := newTestPanel(t)
	ts := httptest.NewServer(srv.AgentHandler())
	defer ts.Close()

	existing, err := st.CreateNode("web-1", "host1", "linux", "amd64", "v1")
	if err != nil {
		t.Fatal(err)
	}

	// No enrollment token configured, and no node token presented: a same-name
	// registration must NOT take over the existing record.
	var got registerResponse
	doJSON(t, http.DefaultClient, http.MethodPost, ts.URL+"/api/agent/register",
		map[string]string{"name": "web-1", "hostname": "attacker"}, http.StatusOK, &got)
	if got.NodeID == existing.ID {
		t.Fatal("registration hijacked an existing node without proof of ownership")
	}

	// Presenting the node's own token allows a legitimate re-registration,
	// which reuses the record and mints a refreshed token.
	body, _ := json.Marshal(map[string]string{"name": "web-1", "hostname": "host1"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/agent/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Node-Token", existing.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	var again registerResponse
	_ = json.NewDecoder(resp.Body).Decode(&again)
	if again.NodeID != existing.ID {
		t.Fatalf("token-bearing re-register created a new node (%s != %s)", again.NodeID, existing.ID)
	}
	if again.Token == "" || again.Token == existing.Token {
		t.Fatal("re-register should mint a fresh token")
	}
}

func TestAgentRegisterRecordsConnectionIP(t *testing.T) {
	st, srv := newTestPanel(t)
	ts := httptest.NewServer(srv.AgentHandler())
	defer ts.Close()

	var got registerResponse
	doJSON(t, http.DefaultClient, http.MethodPost, ts.URL+"/api/agent/register",
		map[string]string{"name": "ip-node", "hostname": "h"}, http.StatusOK, &got)

	n, err := st.GetNode(got.NodeID)
	if err != nil {
		t.Fatal(err)
	}
	// httptest serves on 127.0.0.1, so the observed peer address must be that.
	if n.IP != "127.0.0.1" {
		t.Fatalf("recorded IP = %q, want 127.0.0.1", n.IP)
	}
}

func TestTwoFAEnrollmentAndLogin(t *testing.T) {
	_, srv := newTestPanel(t)
	ts := httptest.NewServer(srv.WebHandler())
	defer ts.Close()

	client := newJarClient(t)
	var login struct {
		OK      bool   `json:"ok"`
		Need2FA bool   `json:"need_2fa"`
		User    string `json:"user"`
	}
	doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, http.StatusOK, &login)
	if !login.OK {
		t.Fatalf("password login failed: %+v", login)
	}

	var setup twoFASetupResponse
	doJSON(t, client, http.MethodPost, ts.URL+"/api/settings/2fa/setup",
		map[string]string{"password": "admin"}, http.StatusOK, &setup)
	if setup.Secret == "" || setup.OTPAuth == "" {
		t.Fatalf("setup returned empty secret/otpauth: %+v", setup)
	}

	// A wrong confirmation code must not enable the factor.
	doJSON(t, client, http.MethodPost, ts.URL+"/api/settings/2fa/enable",
		map[string]string{"password": "admin", "code": "000000"}, http.StatusBadRequest, nil)

	var enabled twoFAEnableResponse
	doJSON(t, client, http.MethodPost, ts.URL+"/api/settings/2fa/enable",
		map[string]string{"password": "admin", "code": totpAt(setup.Secret, time.Now())}, http.StatusOK, &enabled)
	if len(enabled.RecoveryCodes) == 0 {
		t.Fatal("no recovery codes returned")
	}

	// A fresh client now needs the second factor.
	c2 := newJarClient(t)
	var step struct {
		OK        bool   `json:"ok"`
		Need2FA   bool   `json:"need_2fa"`
		Challenge string `json:"challenge"`
	}
	doJSON(t, c2, http.MethodPost, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, http.StatusOK, &step)
	if !step.Need2FA || step.Challenge == "" || step.OK {
		t.Fatalf("expected a 2FA challenge, got %+v", step)
	}

	// Wrong code -> 401.
	doJSON(t, c2, http.MethodPost, ts.URL+"/api/auth/login/2fa",
		map[string]string{"challenge": step.Challenge, "code": "111111"}, http.StatusUnauthorized, nil)

	// A valid TOTP completes the login.
	var done struct {
		OK bool `json:"ok"`
	}
	doJSON(t, c2, http.MethodPost, ts.URL+"/api/auth/login/2fa",
		map[string]string{"challenge": step.Challenge, "code": totpAt(setup.Secret, time.Now())}, http.StatusOK, &done)
	if !done.OK {
		t.Fatal("2FA login failed")
	}

	// A recovery code works once, then is rejected.
	recovery := enabled.RecoveryCodes[0]
	c3 := newJarClient(t)
	var s3 struct {
		Challenge string `json:"challenge"`
	}
	doJSON(t, c3, http.MethodPost, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, http.StatusOK, &s3)
	doJSON(t, c3, http.MethodPost, ts.URL+"/api/auth/login/2fa",
		map[string]string{"challenge": s3.Challenge, "code": recovery}, http.StatusOK, nil)

	c4 := newJarClient(t)
	var s4 struct {
		Challenge string `json:"challenge"`
	}
	doJSON(t, c4, http.MethodPost, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, http.StatusOK, &s4)
	doJSON(t, c4, http.MethodPost, ts.URL+"/api/auth/login/2fa",
		map[string]string{"challenge": s4.Challenge, "code": recovery}, http.StatusUnauthorized, nil)
}

func TestPasswordChangeRequiresCurrent(t *testing.T) {
	_, srv := newTestPanel(t)
	ts := httptest.NewServer(srv.WebHandler())
	defer ts.Close()

	client := newJarClient(t)
	doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, http.StatusOK, nil)

	// Wrong current password is rejected and the old password still works.
	doJSON(t, client, http.MethodPost, ts.URL+"/api/settings/password",
		map[string]string{"username": "admin", "current_password": "nope", "password": "newpassword"}, http.StatusUnauthorized, nil)

	c2 := newJarClient(t)
	doJSON(t, c2, http.MethodPost, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "admin"}, http.StatusOK, nil)

	// Correct current password succeeds and issues a fresh session.
	doJSON(t, client, http.MethodPost, ts.URL+"/api/settings/password",
		map[string]string{"username": "admin", "current_password": "admin", "password": "newpassword"}, http.StatusOK, nil)

	c3 := newJarClient(t)
	doJSON(t, c3, http.MethodPost, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "newpassword"}, http.StatusOK, nil)
}
