package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/BingyanStudio/CloudPilot/internal/model"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/labstack/echo/v5"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

const oidcCookie = "cp_oidc"

type oidcSettingsView struct {
	Enabled         bool   `json:"enabled"`
	DiscoveryURL    string `json:"discoveryUrl"`
	ClientID        string `json:"clientId"`
	ClientSecret    string `json:"clientSecret,omitempty"`
	HasClientSecret bool   `json:"hasClientSecret"`
	Scopes          string `json:"scopes"`
	DefaultRole     string `json:"defaultRole"`
	RedirectURL     string `json:"redirectUrl"`
}

func (a *App) oidcSettings() (model.OIDCSettings, error) {
	settings := model.OIDCSettings{ID: 1, Scopes: "openid profile email", DefaultRole: model.RoleViewer}
	err := a.DB.Where("id = ?", 1).First(&settings).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
	}
	return settings, err
}

func (a *App) oidcRedirectURL(c *echo.Context) string {
	base := a.PublicURL
	if base == "" {
		base = c.Scheme() + "://" + c.Request().Host
	}
	return base + "/api/auth/oidc/callback"
}

// OIDCStatus tells the login page whether SSO is available.
func (a *App) OIDCStatus(c *echo.Context) error {
	settings, err := a.oidcSettings()
	if err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"enabled": settings.Enabled})
}

// OIDCSettings returns the SSO configuration without the client secret.
func (a *App) OIDCSettings(c *echo.Context) error {
	settings, err := a.oidcSettings()
	if err != nil {
		return internal(c, err)
	}
	return c.JSON(http.StatusOK, oidcSettingsView{
		Enabled: settings.Enabled, DiscoveryURL: settings.DiscoveryURL, ClientID: settings.ClientID,
		HasClientSecret: settings.ClientSecret != "", Scopes: settings.Scopes, DefaultRole: settings.DefaultRole,
		RedirectURL: a.oidcRedirectURL(c),
	})
}

// SaveOIDCSettings validates the provider's discovery document before enabling SSO.
// An empty client secret keeps the stored one.
func (a *App) SaveOIDCSettings(c *echo.Context) error {
	var body oidcSettingsView
	if err := decodeBody(c, &body); err != nil {
		return fail(c, http.StatusBadRequest, err.Error())
	}
	settings, err := a.oidcSettings()
	if err != nil {
		return internal(c, err)
	}
	scopes := strings.Fields(strings.ReplaceAll(body.Scopes, ",", " "))
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}
	if model.RoleRank(body.DefaultRole) == 0 {
		return fail(c, http.StatusBadRequest, "defaultRole must be admin, editor or viewer")
	}
	settings.Enabled, settings.DiscoveryURL, settings.ClientID = body.Enabled, strings.TrimSpace(body.DiscoveryURL), strings.TrimSpace(body.ClientID)
	settings.Scopes, settings.DefaultRole = strings.Join(scopes, " "), body.DefaultRole
	if body.ClientSecret != "" {
		if settings.ClientSecret, err = model.Encrypt(a.Encryption, "OIDC", body.ClientSecret); err != nil {
			return internal(c, err)
		}
	}
	if settings.Enabled {
		if settings.ClientID == "" || settings.ClientSecret == "" {
			return fail(c, http.StatusBadRequest, "client ID and client secret are required to enable SSO")
		}
		if _, err := discoverOIDC(c.Request().Context(), settings.DiscoveryURL); err != nil {
			return fail(c, http.StatusBadRequest, err.Error())
		}
	}
	if err := a.DB.Save(&settings).Error; err != nil {
		return internal(c, err)
	}
	return a.OIDCSettings(c)
}

// discoverOIDC fetches a provider configuration so the issuer always matches what the provider publishes.
func discoverOIDC(ctx context.Context, discoveryURL string) (*oidc.Provider, error) {
	u, err := url.Parse(discoveryURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, errors.New("discovery URL must be an absolute HTTP(S) URL")
	}
	if !strings.HasSuffix(u.Path, "/.well-known/openid-configuration") {
		u.Path = strings.TrimRight(u.Path, "/") + "/.well-known/openid-configuration"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch OIDC discovery document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch OIDC discovery document: HTTP %d", resp.StatusCode)
	}
	var config oidc.ProviderConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&config); err != nil {
		return nil, fmt.Errorf("invalid OIDC discovery document: %w", err)
	}
	if config.IssuerURL == "" || config.AuthURL == "" || config.TokenURL == "" || config.JWKSURL == "" {
		return nil, errors.New("OIDC discovery document lacks issuer, authorization, token or JWKS endpoints")
	}
	// The keyset fetches lazily and must outlive this request's timeout.
	return config.NewProvider(context.Background()), nil
}

func (a *App) oidcClient(c *echo.Context) (model.OIDCSettings, *oidc.Provider, *oauth2.Config, error) {
	settings, err := a.oidcSettings()
	if err != nil || !settings.Enabled {
		return settings, nil, nil, errors.New("SSO is not enabled")
	}
	secret, err := model.Decrypt(a.Encryption, "OIDC", settings.ClientSecret)
	if err != nil {
		return settings, nil, nil, errors.New("SSO client secret is unavailable")
	}
	provider, err := discoverOIDC(c.Request().Context(), settings.DiscoveryURL)
	if err != nil {
		return settings, nil, nil, err
	}
	return settings, provider, &oauth2.Config{
		ClientID: settings.ClientID, ClientSecret: secret, Endpoint: provider.Endpoint(),
		RedirectURL: a.oidcRedirectURL(c), Scopes: strings.Fields(settings.Scopes),
	}, nil
}

// OIDCLogin redirects the browser to the identity provider with state, nonce and PKCE bound to a short-lived cookie.
func (a *App) OIDCLogin(c *echo.Context) error {
	_, _, config, err := a.oidcClient(c)
	if err != nil {
		return ssoFailure(c, err.Error())
	}
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	c.SetCookie(&http.Cookie{Name: oidcCookie, Value: state + "." + nonce + "." + verifier, Path: "/api/auth/oidc", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	return c.Redirect(http.StatusFound, config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)))
}

// OIDCCallback verifies the ID token, provisions first-time users with the default role and starts a session.
func (a *App) OIDCCallback(c *echo.Context) error {
	cookie, err := c.Cookie(oidcCookie)
	c.SetCookie(&http.Cookie{Name: oidcCookie, Path: "/api/auth/oidc", HttpOnly: true, Secure: a.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	if err != nil {
		return ssoFailure(c, "SSO login expired; please retry")
	}
	parts := strings.Split(cookie.Value, ".")
	state := c.QueryParam("state")
	if len(parts) != 3 || state == "" || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(state)) != 1 {
		return ssoFailure(c, "SSO state mismatch; please retry")
	}
	if message := c.QueryParam("error"); message != "" {
		return ssoFailure(c, "identity provider rejected the login: "+message)
	}
	settings, provider, config, err := a.oidcClient(c)
	if err != nil {
		return ssoFailure(c, err.Error())
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 15*time.Second)
	defer cancel()
	token, err := config.Exchange(ctx, c.QueryParam("code"), oauth2.VerifierOption(parts[2]))
	if err != nil {
		return ssoFailure(c, "SSO code exchange failed")
	}
	rawID, _ := token.Extra("id_token").(string)
	idToken, err := provider.Verifier(&oidc.Config{ClientID: settings.ClientID}).Verify(ctx, rawID)
	if err != nil || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(parts[1])) != 1 {
		return ssoFailure(c, "SSO ID token is invalid")
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
		Name              string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return ssoFailure(c, "SSO ID token is invalid")
	}
	user, err := a.oidcUser(idToken.Issuer, idToken.Subject, settings.DefaultRole, claims.PreferredUsername, claims.Email, claims.Name)
	if err != nil {
		c.Logger().Error("provision SSO user", "error", err)
		return ssoFailure(c, "SSO account could not be created")
	}
	if err := a.startSession(c, user); err != nil {
		return internal(c, err)
	}
	return c.Redirect(http.StatusFound, "/")
}

// oidcUser finds the account bound to an issuer and subject, or creates one. SSO never links to existing local accounts.
func (a *App) oidcUser(issuer, subject, role string, names ...string) (model.User, error) {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	key := hex.EncodeToString(sum[:])
	var user model.User
	err := a.DB.Where("oidc_subject = ?", key).First(&user).Error
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return user, err
	}
	username := subject
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			username = name
			break
		}
	}
	if len(username) > 180 {
		username = username[:180]
	}
	for _, candidate := range []string{username, username + "-" + key[:6]} {
		user = model.User{Username: candidate, Role: role, OIDCSubject: &key}
		if err = a.DB.Create(&user).Error; !errors.Is(err, gorm.ErrDuplicatedKey) {
			user.SSO = true
			return user, err
		}
	}
	return user, err
}

func ssoFailure(c *echo.Context, message string) error {
	return c.Redirect(http.StatusFound, "/?sso_error="+url.QueryEscape(message))
}

func randomToken() string {
	data := make([]byte, 24)
	_, _ = rand.Read(data)
	return hex.EncodeToString(data)
}
