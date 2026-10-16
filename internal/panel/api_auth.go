package panel

import (
	"fmt"
	"net/http"
	"time"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	OK        bool      `json:"ok"`
	Need2FA   bool      `json:"need_2fa,omitempty"`
	Challenge string    `json:"challenge,omitempty"`
	User      string    `json:"user,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

const sessionTTL = 7 * 24 * time.Hour

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	key := clientIP(r)
	if ok, wait := s.login.allow(key); !ok {
		writeErr(w, http.StatusTooManyRequests, "尝试过于频繁，请在 "+humanWait(wait)+"后重试")
		return
	}
	if !s.store.CheckAdmin(req.Username, req.Password) {
		s.login.fail(key)
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	if s.store.Settings().TwoFAEnabled {
		// Password accepted, but the session is withheld until the second
		// factor is presented.
		writeOK(w, loginResponse{
			OK:        false,
			Need2FA:   true,
			Challenge: s.signChallenge(time.Now().Add(5 * time.Minute)),
		})
		return
	}
	s.login.reset(key)
	s.finishLogin(w, req.Username)
}

type twoFARequest struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

func (s *Server) handleLoginTwoFA(w http.ResponseWriter, r *http.Request) {
	var req twoFARequest
	if !decodeJSON(w, r, &req) {
		return
	}
	key := clientIP(r)
	if ok, wait := s.login.allow(key); !ok {
		writeErr(w, http.StatusTooManyRequests, "尝试过于频繁，请在 "+humanWait(wait)+"后重试")
		return
	}
	if !s.verifyChallenge(req.Challenge) {
		writeErr(w, http.StatusUnauthorized, "验证已过期，请重新登录")
		return
	}
	set := s.store.Settings()
	if !set.TwoFAEnabled {
		s.login.reset(key)
		s.finishLogin(w, set.AdminUser)
		return
	}
	if verifyTOTP(set.TwoFASecret, req.Code, time.Now()) {
		s.login.reset(key)
		s.finishLogin(w, set.AdminUser)
		return
	}
	if h := s.store.HashRecoveryCode(req.Code); s.store.ConsumeRecoveryHash(h) {
		s.login.reset(key)
		s.finishLogin(w, set.AdminUser)
		return
	}
	s.login.fail(key)
	writeErr(w, http.StatusUnauthorized, "验证码错误")
}

// finishLogin issues the session cookie and reports success.
func (s *Server) finishLogin(w http.ResponseWriter, user string) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.signSession(user, exp),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	writeOK(w, loginResponse{OK: true, User: user, ExpiresAt: exp})
}

// handleMe reports the signed-in account plus a little panel metadata the UI
// needs (notably the agent listener URL used to build install commands).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	set := s.store.Settings()
	writeOK(w, map[string]any{
		"user":       set.AdminUser,
		"twofa":      set.TwoFAEnabled,
		"agent_url":  s.agentBaseURL(r),
		"version":    s.version,
		"cookie_ssl": s.cookieSecure,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeOK(w, map[string]bool{"ok": true})
}

func humanWait(d time.Duration) string {
	if d < time.Second {
		return "稍后"
	}
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds())+1)
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes())+1)
}
