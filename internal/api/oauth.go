// Package api's oauth.go implements this service's own GitHub OAuth
// authorization-code flow: /auth/login redirects to GitHub, /auth/callback
// exchanges the code, verifies org membership, and mints the same signed
// agentevals_session cookie sessionauth.go already knows how to validate.
//
// Ported from auth/config.py's GitHubOAuthConfig/make_session_cookie/
// make_state_token/verify_state and auth/routes.py's login/callback/logout
// handlers, but this service now runs the *entire* flow itself rather than
// only trusting a cookie a separate Python process minted - this service
// is meant to fully replace that Python deployment, not run alongside it
// forever, so it can no longer depend on that process being up just to
// let a user log in. One deliberate simplification vs. Python: the
// callback redirect_uri is always PublicURL+"/auth/callback" (a required
// config value), never derived from the incoming request's Host/proto
// headers (Python's request.url_for, which its own comment notes is
// fragile behind a proxy that doesn't forward X-Forwarded-Proto) - this
// port has one production URL, not several environments needing that
// flexibility.
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHubOAuthConfig holds this service's own GitHub OAuth App credentials
// and the org whose active membership gates access. Constructing one (via
// GitHubOAuthConfigFromParams) is what turns the auth gate on; see Serve.
type GitHubOAuthConfig struct {
	ClientID      string
	ClientSecret  string
	Org           string
	SessionSecret string
	PublicURL     string // e.g. "https://your-app.example.com", no trailing slash
}

// sessionTTL / stateTTL match auth/config.py's SESSION_TTL_SECONDS (12h -
// GitHub org membership is re-checked on next login, not per-request) and
// STATE_TTL_SECONDS (10m, just long enough for a human to complete the
// GitHub-hosted part of the redirect).
const (
	sessionTTL = 12 * time.Hour
	stateTTL   = 10 * time.Minute

	stateCookieName = "agentevals_oauth_state"
)

// githubAuthorizeURL/githubTokenURL/githubAPIBaseURL are vars, not consts,
// solely so tests can point them at an httptest.Server instead of real
// GitHub.
var (
	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
	githubAPIBaseURL   = "https://api.github.com"
)

// GitHubOAuthConfigFromParams ports GitHubOAuthConfig.from_env's
// validation: a present clientID/clientSecret is what turns the gate on
// (returns nil, nil if both are empty - auth stays disabled, the
// dev/local default); once on, org/sessionSecret/publicURL are mandatory
// and its absence is a startup error, not a silently-open gate - an
// org-less GitHub OAuth setup authenticates any GitHub user but doesn't
// authorize anyone in particular.
func GitHubOAuthConfigFromParams(clientID, clientSecret, org, sessionSecret, publicURL string) (*GitHubOAuthConfig, error) {
	if clientID == "" || clientSecret == "" {
		return nil, nil
	}
	if org == "" {
		return nil, fmt.Errorf("github client id/secret are set but no org was given: an org-less GitHub OAuth gate authenticates but does not authorize - refusing to start rather than silently letting any GitHub user in")
	}
	if sessionSecret == "" {
		return nil, fmt.Errorf("github client id is set but no session secret was given")
	}
	publicURL = strings.TrimRight(publicURL, "/")
	if publicURL == "" {
		return nil, fmt.Errorf("github client id is set but no public URL was given: this service needs an exact redirect_uri to send GitHub, and derives no fallback from the incoming request (see oauth.go's package doc)")
	}
	return &GitHubOAuthConfig{
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		Org:           org,
		SessionSecret: sessionSecret,
		PublicURL:     publicURL,
	}, nil
}

func (c *GitHubOAuthConfig) callbackURL() string {
	return c.PublicURL + "/auth/callback"
}

// mintSessionCookie ports make_session_cookie: username|expiry, HMAC-SHA256
// signed, urlsafe-base64 encoded - the exact format readSessionCookie
// already validates.
func mintSessionCookie(username, secret string) string {
	return MintSessionToken(username, secret, sessionTTL)
}

// MintSessionToken signs a token in the same username|expiry|hmac format
// mintSessionCookie/readSessionCookie use, valid for ttl from now. Exported
// so cmd/agentevals's `auth mint-token` subcommand (ported from cli.py's
// mint_token) can produce a long-lived bearer token for non-browser
// clients - an OTLP exporter, or the MCP server (cmd/agentevals/mcp.go)
// calling a GitHub-OAuth-gated `agentevals serve` deployment - that have no
// interactive login flow of their own. Accepted anywhere a session cookie
// is: requireSession's Authorization: Bearer path, or the agentevals_session
// cookie itself.
func MintSessionToken(username, secret string, ttl time.Duration) string {
	expiry := time.Now().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%d", username, expiry)
	sig := signSessionPayload(secret, payload)
	return base64.URLEncoding.EncodeToString([]byte(payload + "|" + sig))
}

// makeStateToken is an opaque CSRF nonce: set as an HttpOnly cookie before
// redirecting to GitHub, echoed back verbatim as ?state=, and only ever
// compared for equality against its own cookie (see verifyState) - unlike
// the session cookie, it needs no signature or cross-service portability,
// since this service both sets and checks it.
func makeStateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// verifyState ports verify_state's constant-time equality check.
func verifyState(cookieValue, queryValue string) bool {
	if cookieValue == "" || queryValue == "" {
		return false
	}
	return hmac.Equal([]byte(cookieValue), []byte(queryValue))
}

// oauthCookieOpts mirrors auth/routes.py's shared _COOKIE_KWARGS: this
// service only ever runs behind the gateway's TLS termination, so Secure
// is safe unconditionally.
func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func deleteCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
}

// authLoginHandler implements GET /auth/login: ports auth/routes.py's
// login. Redirects to GitHub's OAuth authorize endpoint with a freshly
// minted CSRF state, which is also stashed in a short-lived cookie for
// authCallbackHandler to check against GitHub's echoed-back ?state=.
func authLoginHandler(cfg *GitHubOAuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, err := makeStateToken()
		if err != nil {
			http.Error(w, "failed to generate OAuth state", http.StatusInternalServerError)
			return
		}
		setCookie(w, stateCookieName, state, stateTTL)
		params := url.Values{
			"client_id":    {cfg.ClientID},
			"redirect_uri": {cfg.callbackURL()},
			"scope":        {"read:org"},
			"state":        {state},
		}
		http.Redirect(w, r, githubAuthorizeURL+"?"+params.Encode(), http.StatusFound)
	}
}

// authCallbackHandler implements GET /auth/callback: ports auth/routes.py's
// callback. Exchanges the code for an access token, looks up the
// authenticated user's login, then checks their membership in cfg.Org -
// per GitHub's API, only a 204 response means "confirmed active member";
// a 302 (membership exists but this token can't see it) or 404 (not a
// member) must both deny, since "can't tell" is not a safe default for an
// access gate. On success, mints and sets the same signed session cookie
// requireSession validates, then redirects to /evals (this service's own
// canonical UI path now - see deploy/k8s.yaml's HTTPRoute).
func authCallbackHandler(cfg *GitHubOAuthConfig) http.HandlerFunc {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		cookieState := ""
		if c, err := r.Cookie(stateCookieName); err == nil {
			cookieState = c.Value
		}
		if code == "" || !verifyState(cookieState, state) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or expired OAuth state"})
			return
		}

		accessToken, err := exchangeCodeForToken(client, cfg, code)
		if err != nil {
			log.Printf("auth: GitHub OAuth token exchange failed: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub OAuth token exchange failed"})
			return
		}

		username, err := fetchGitHubLogin(client, accessToken)
		if err != nil {
			log.Printf("auth: fetching GitHub user failed: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to fetch GitHub user"})
			return
		}

		member, err := checkOrgMembership(client, cfg.Org, username, accessToken)
		if err != nil {
			log.Printf("auth: checking org membership failed: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to verify org membership"})
			return
		}
		if !member {
			log.Printf("auth: GitHub OAuth: %s denied (not an active member of org %s)", username, cfg.Org)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("GitHub user %q is not an active member of the %q org", username, cfg.Org)})
			return
		}

		log.Printf("auth: GitHub OAuth: %s authenticated (member of %s)", username, cfg.Org)
		setCookie(w, sessionCookieName, mintSessionCookie(username, cfg.SessionSecret), sessionTTL)
		deleteCookie(w, stateCookieName)
		http.Redirect(w, r, "/evals", http.StatusFound)
	}
}

// authLogoutHandler implements GET /auth/logout, ported from
// auth/routes.py's logout: clears the session cookie and returns to /evals.
func authLogoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deleteCookie(w, sessionCookieName)
		http.Redirect(w, r, "/evals", http.StatusFound)
	}
}

func exchangeCodeForToken(client *http.Client, cfg *GitHubOAuthConfig, code string) (string, error) {
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {cfg.callbackURL()},
	}
	req, err := http.NewRequest(http.MethodPost, githubTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub token endpoint returned %d", resp.StatusCode)
	}

	var data struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.AccessToken == "" {
		return "", fmt.Errorf("no access_token in response (error=%q)", data.Error)
	}
	return data.AccessToken, nil
}

func fetchGitHubLogin(client *http.Client, accessToken string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, githubAPIBaseURL+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub user endpoint returned %d", resp.StatusCode)
	}

	var data struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.Login == "" {
		return "", fmt.Errorf("GitHub user endpoint returned no login")
	}
	return data.Login, nil
}

// checkOrgMembership ports the callback's membership check: GitHub
// returns 204 for a confirmed active member, and this port intentionally
// treats any other status (302 = membership exists but is not public and
// this token can't see it, 404 = not a member, anything else = error) as
// "not a member" - "can't tell" must deny, not admit, for an access gate.
func checkOrgMembership(client *http.Client, org, username, accessToken string) (bool, error) {
	membershipURL := fmt.Sprintf("%s/orgs/%s/members/%s", githubAPIBaseURL, url.PathEscape(org), url.PathEscape(username))
	req, err := http.NewRequest(http.MethodGet, membershipURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent, nil
}
