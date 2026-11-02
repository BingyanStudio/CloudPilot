package views_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/controller"
	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/BingyanStudio/CloudPilot/internal/views"
	"github.com/labstack/echo/v5"
)

func TestRolesUsersAndOIDCLogin(t *testing.T) {
	dir := t.TempDir()
	db, err := model.Open("sqlite", filepath.Join(dir, "cloudpilot.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := model.BootstrapAdmin(db, "admin", "test-password-123"); err != nil {
		t.Fatal(err)
	}
	encryption, err := model.Unlock(db, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	views.Register(e, &controller.App{DB: db, DataDir: dir, Encryption: encryption, PublicURL: "http://cloudpilot.test"})
	call := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, req)
		return recorder
	}
	sessionCookie := func(r *httptest.ResponseRecorder) *http.Cookie {
		t.Helper()
		for _, cookie := range r.Result().Cookies() {
			if cookie.Name == "cp_session" {
				return cookie
			}
		}
		t.Fatalf("no session cookie: %d %s", r.Code, r.Body)
		return nil
	}
	login := func(username string) *http.Cookie {
		return sessionCookie(call("POST", "/api/auth/login", `{"username":"`+username+`","password":"test-password-123"}`, nil))
	}
	admin := login("admin")
	for _, role := range []string{"editor", "viewer"} {
		if r := call("POST", "/api/users", `{"username":"`+role+`","password":"test-password-123","role":"`+role+`"}`, admin); r.Code != 201 {
			t.Fatalf("create %s: %d %s", role, r.Code, r.Body)
		}
	}
	if r := call("POST", "/api/users", `{"username":"editor","password":"test-password-123","role":"viewer"}`, admin); r.Code != 409 {
		t.Fatalf("duplicate user status %d", r.Code)
	}
	editor, viewer := login("editor"), login("viewer")
	runner := `{"apiVersion":"cloudpilot.dev/v1alpha1","kind":"Runner","metadata":{"name":"docker"},"spec":{"type":"docker","image":"cloudpilot-codex:latest"}}`
	for _, check := range []struct {
		name, method, path, body string
		cookie                   *http.Cookie
		want                     int
	}{
		{"viewer reads", "GET", "/api/resources", "", viewer, 200},
		{"viewer cannot edit", "POST", "/api/resources", runner, viewer, 403},
		{"viewer cannot message", "POST", "/api/runs/1/messages", `{"message":"hi"}`, viewer, 403},
		{"editor edits", "POST", "/api/resources", runner, editor, 200},
		{"editor cannot manage users", "GET", "/api/users", "", editor, 403},
		{"admin cannot delete self", "DELETE", "/api/users/1", "", admin, 409},
		{"admin cannot demote self", "PATCH", "/api/users/1", `{"role":"viewer"}`, admin, 409},
	} {
		if r := call(check.method, check.path, check.body, check.cookie); r.Code != check.want {
			t.Fatalf("%s: status %d, want %d: %s", check.name, r.Code, check.want, r.Body)
		}
	}
	if r := call("PATCH", "/api/users/3", `{"password":"another-password-1"}`, admin); r.Code != 200 {
		t.Fatalf("reset password: %d %s", r.Code, r.Body)
	}
	if r := call("GET", "/api/auth/me", "", viewer); r.Code != 401 {
		t.Fatalf("password reset kept session alive: %d", r.Code)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	var nonce string
	idp := httptest.NewServer(nil)
	t.Cleanup(idp.Close)
	idp.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": idp.URL, "authorization_endpoint": idp.URL + "/auth", "token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/keys"})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
		case "/token":
			claims, _ := json.Marshal(map[string]any{"iss": idp.URL, "sub": "subject-1", "aud": "cloudpilot", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce, "preferred_username": "alice"})
			signed := b64([]byte(`{"alg":"RS256","kid":"k1","typ":"JWT"}`)) + "." + b64(claims)
			digest := sha256.Sum256([]byte(signed))
			signature, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "expires_in": 3600, "id_token": signed + "." + b64(signature)})
		default:
			http.NotFound(w, r)
		}
	})
	if r := call("PUT", "/api/settings/oidc", `{"enabled":true,"discoveryUrl":"`+idp.URL+`","clientId":"cloudpilot","clientSecret":"secret","scopes":"profile","defaultRole":"editor"}`, admin); r.Code != 200 || strings.Contains(r.Body.String(), `"secret"`) {
		t.Fatalf("save OIDC settings: %d %s", r.Code, r.Body)
	}
	for range 2 {
		start := call("GET", "/api/auth/oidc/login", "", nil)
		location, err := url.Parse(start.Header().Get("Location"))
		if start.Code != 302 || err != nil || location.Query().Get("code_challenge") == "" || location.Query().Get("redirect_uri") != "http://cloudpilot.test/api/auth/oidc/callback" {
			t.Fatalf("OIDC login redirect: %d %s", start.Code, start.Header().Get("Location"))
		}
		nonce = location.Query().Get("nonce")
		if r := call("GET", "/api/auth/oidc/callback?code=c&state=forged", "", start.Result().Cookies()[0]); !strings.Contains(r.Header().Get("Location"), "sso_error") {
			t.Fatalf("forged state accepted: %s", r.Header().Get("Location"))
		}
		callback := call("GET", "/api/auth/oidc/callback?code=c&state="+location.Query().Get("state"), "", start.Result().Cookies()[0])
		if callback.Header().Get("Location") != "/" {
			t.Fatalf("OIDC callback: %d %s", callback.Code, callback.Header().Get("Location"))
		}
		me := call("GET", "/api/auth/me", "", sessionCookie(callback))
		if !strings.Contains(me.Body.String(), `"username":"alice","role":"editor","sso":true`) {
			t.Fatalf("SSO user: %s", me.Body)
		}
	}
	var count int64
	db.Model(&model.User{}).Where("username LIKE ?", "alice%").Count(&count)
	if count != 1 {
		t.Fatalf("repeated SSO login created %d accounts", count)
	}
}
