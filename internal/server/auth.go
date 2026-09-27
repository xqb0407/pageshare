package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
)

// admin 包一层 Bearer token 校验（constant-time 比较）。
func (s *Server) admin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.Cfg.AuthToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pageshare"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要有效的 Bearer token"})
			return
		}
		next(w, r)
	})
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
