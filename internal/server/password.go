package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func hashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// gateCookieName 是某站点的密码会话 cookie 名。
func gateCookieName(siteID string) string { return "psg_" + strings.ToLower(siteID) }

// gateToken 生成签名会话值 "<expires_unix>.<hmac(siteID|expires)>"，无服务端状态。
func (s *Server) gateToken(siteID string, until time.Time) string {
	exp := strconv.FormatInt(unix(until), 10)
	mac := hmac.New(sha256.New, []byte(s.Cfg.CookieSecret))
	mac.Write([]byte(siteID + "|" + exp))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return exp + "." + sig
}

// gateTokenValid 校验签名且未过期。
func (s *Server) gateTokenValid(siteID, token string) bool {
	expStr, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.Cfg.CookieSecret))
	mac.Write([]byte(siteID + "|" + expStr))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(want))
}

func unix(t time.Time) int64 { return t.Unix() }
