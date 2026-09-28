package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDCSettings is the generic OpenID Connect configuration: an issuer that publishes a discovery
// document, a client, and the checks that decide who may sign in. Plain values, so it can live in a
// config struct without dragging a mutex along.
type OIDCSettings struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	AllowedDomain string // every email must be at this domain (e.g. opsavor.ai); empty allows any
	Role          Role   // the role a newly seen user is created with
	Scopes        []string
	Label         string // what the sign-in button says, e.g. "Google Workspace"
}

// OIDCConfig is a configured client: the settings plus the discovery cache.
type OIDCConfig struct {
	OIDCSettings

	client    *http.Client
	mu        sync.Mutex
	provider  *oidcProvider
	fetchedAt time.Time
}

type oidcProvider struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// NewOIDC validates settings and returns a client ready to use.
func NewOIDC(s OIDCSettings) (*OIDCConfig, error) {
	if strings.TrimSpace(s.Issuer) == "" || s.ClientID == "" || s.ClientSecret == "" || s.RedirectURL == "" {
		return nil, errors.New("OIDC needs an issuer, client id, client secret and redirect url")
	}
	if !ValidRole(s.Role) {
		s.Role = RoleViewer
	}
	if len(s.Scopes) == 0 {
		s.Scopes = []string{"openid", "email", "profile"}
	}
	if s.Label == "" {
		s.Label = "single sign-on"
	}
	return &OIDCConfig{OIDCSettings: s}, nil
}

func (o *OIDCConfig) httpClient() *http.Client {
	if o.client != nil {
		return o.client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Provider fetches and caches the issuer's discovery document.
func (o *OIDCConfig) Provider(ctx context.Context) (*oidcProvider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider != nil && time.Since(o.fetchedAt) < time.Hour {
		return o.provider, nil
	}
	endpoint := strings.TrimRight(o.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	res, err := o.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery answered %d", res.StatusCode)
	}
	var p oidcProvider
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&p); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if p.AuthorizationEndpoint == "" || p.TokenEndpoint == "" {
		return nil, errors.New("oidc discovery did not name an authorization and token endpoint")
	}
	o.provider, o.fetchedAt = &p, time.Now()
	return o.provider, nil
}

// AuthorizeURL is where the browser is sent to sign in.
func (o *OIDCConfig) AuthorizeURL(ctx context.Context, state string) (string, error) {
	p, err := o.Provider(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {o.ClientID},
		"redirect_uri":  {o.RedirectURL},
		"scope":         {strings.Join(o.Scopes, " ")},
		"state":         {state},
	}
	return p.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Exchange trades an authorization code for an access token.
func (o *OIDCConfig) Exchange(ctx context.Context, code string) (string, error) {
	p, err := o.Provider(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {o.RedirectURL},
		"client_id":     {o.ClientID},
		"client_secret": {o.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := o.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc token exchange: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc token exchange answered %d", res.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("oidc token exchange returned no access token")
	}
	return tok.AccessToken, nil
}

// OIDCIdentity is who the provider says signed in.
type OIDCIdentity struct {
	Email string
	Name  string
}

// Identity calls the provider's userinfo endpoint and applies the checks that decide access: the email
// must be present and verified, and at the allowed domain where one is configured.
func (o *OIDCConfig) Identity(ctx context.Context, accessToken string) (OIDCIdentity, error) {
	p, err := o.Provider(ctx)
	if err != nil {
		return OIDCIdentity{}, err
	}
	if p.UserinfoEndpoint == "" {
		return OIDCIdentity{}, errors.New("the provider publishes no userinfo endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserinfoEndpoint, nil)
	if err != nil {
		return OIDCIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	res, err := o.httpClient().Do(req)
	if err != nil {
		return OIDCIdentity{}, fmt.Errorf("oidc userinfo: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return OIDCIdentity{}, fmt.Errorf("oidc userinfo answered %d", res.StatusCode)
	}
	var u struct {
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&u); err != nil {
		return OIDCIdentity{}, fmt.Errorf("oidc userinfo: %w", err)
	}
	email := strings.ToLower(strings.TrimSpace(u.Email))
	if email == "" {
		return OIDCIdentity{}, errors.New("the provider returned no email")
	}
	if !truthy(u.EmailVerified) {
		return OIDCIdentity{}, errors.New("the provider says the email is not verified")
	}
	if o.AllowedDomain != "" && !strings.HasSuffix(email, "@"+strings.ToLower(o.AllowedDomain)) {
		return OIDCIdentity{}, fmt.Errorf("email %s is not at %s", email, o.AllowedDomain)
	}
	return OIDCIdentity{Email: email, Name: u.Name}, nil
}

// truthy accepts the boolean and the string form of an OIDC claim.
func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	}
	return false
}
