package panel

import (
	"net/http"
	"time"
)

const totpIssuer = "opback"

// verifyPassword re-checks the admin password for sensitive operations.
func (s *Server) verifyPassword(password string) bool {
	set := s.store.Settings()
	return s.store.CheckAdmin(set.AdminUser, password)
}

type twoFASetupRequest struct {
	Password string `json:"password"`
}

type twoFASetupResponse struct {
	Secret  string `json:"secret"`
	OTPAuth string `json:"otpauth"`
}

// handleTwoFASetup generates a pending TOTP seed. The factor is not active
// until the operator confirms it via /2fa/enable.
func (s *Server) handleTwoFASetup(w http.ResponseWriter, r *http.Request) {
	var req twoFASetupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.verifyPassword(req.Password) {
		writeErr(w, http.StatusUnauthorized, "当前密码错误")
		return
	}
	secret := newTOTPSecret()
	if secret == "" {
		writeErr(w, http.StatusInternalServerError, "无法生成密钥")
		return
	}
	if err := s.store.SetTwoFAPending(secret); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, twoFASetupResponse{
		Secret:  secret,
		OTPAuth: otpauthURL(totpIssuer, s.store.Settings().AdminUser, secret),
	})
}

type twoFAEnableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

type twoFAEnableResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

// handleTwoFAEnable confirms the pending seed and turns 2FA on, returning the
// one-time recovery codes (shown exactly once).
func (s *Server) handleTwoFAEnable(w http.ResponseWriter, r *http.Request) {
	var req twoFAEnableRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	set := s.store.Settings()
	if !s.verifyPassword(req.Password) {
		writeErr(w, http.StatusUnauthorized, "当前密码错误")
		return
	}
	if set.TwoFAPending == "" {
		writeErr(w, http.StatusBadRequest, "请先生成密钥")
		return
	}
	if !verifyTOTP(set.TwoFAPending, req.Code, time.Now()) {
		writeErr(w, http.StatusBadRequest, "验证码不正确")
		return
	}
	codes := newRecoveryCodes(recoveryCodeCount)
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, s.store.HashRecoveryCode(c))
	}
	if err := s.store.EnableTwoFA(set.TwoFAPending, hashes); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, twoFAEnableResponse{RecoveryCodes: codes})
}

type twoFADisableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// handleTwoFADisable turns 2FA off after re-checking the password. A valid
// TOTP or recovery code is also accepted as proof of possession.
func (s *Server) handleTwoFADisable(w http.ResponseWriter, r *http.Request) {
	var req twoFADisableRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	set := s.store.Settings()
	ok := s.verifyPassword(req.Password)
	if !ok && req.Code != "" {
		if verifyTOTP(set.TwoFASecret, req.Code, time.Now()) {
			ok = true
		} else if h := s.store.HashRecoveryCode(req.Code); s.store.ConsumeRecoveryHash(h) {
			ok = true
		}
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "身份校验失败")
		return
	}
	if err := s.store.DisableTwoFA(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]bool{"ok": true})
}

// handleTwoFARecovery regenerates the recovery codes.
func (s *Server) handleTwoFARecovery(w http.ResponseWriter, r *http.Request) {
	var req twoFASetupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.verifyPassword(req.Password) {
		writeErr(w, http.StatusUnauthorized, "当前密码错误")
		return
	}
	if !s.store.Settings().TwoFAEnabled {
		writeErr(w, http.StatusBadRequest, "尚未启用两步验证")
		return
	}
	codes := newRecoveryCodes(recoveryCodeCount)
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, s.store.HashRecoveryCode(c))
	}
	if err := s.store.ReplaceRecoveryHashes(hashes); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, twoFAEnableResponse{RecoveryCodes: codes})
}
