package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultIssuer   = "https://auth.openai.com"
	DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultAuthPort = 1455
)

type OAuthConfig struct {
	Issuer     string
	ClientID   string
	HTTPClient *http.Client
}

func DefaultOAuthConfig() OAuthConfig {
	return OAuthConfig{
		Issuer:     DefaultIssuer,
		ClientID:   DefaultClientID,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func NeedsRefresh(idToken, accessToken string, now time.Time) bool {
	if strings.TrimSpace(accessToken) == "" {
		return true
	}
	if TokenExpiresWithin(accessToken, now, 60*time.Second) {
		return true
	}
	if strings.TrimSpace(idToken) != "" && TokenExpiresWithin(idToken, now, 60*time.Second) {
		return true
	}
	return false
}

func RefreshTokens(ctx context.Context, cfg OAuthConfig, refreshToken string) (TokenData, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return TokenData{}, errors.New("missing Codex refresh token")
	}
	issuer := strings.TrimRight(cfg.Issuer, "/")
	if issuer == "" {
		issuer = DefaultIssuer
	}
	clientID := cfg.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return TokenData{}, ctx.Err()
			case <-time.After(time.Duration(attempt*400) * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()))
		if err != nil {
			return TokenData{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("Codex token refresh HTTP %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Never retry 4xx errors on single-use refresh tokens.
			return TokenData{}, fmt.Errorf("Codex token refresh rejected (HTTP %d)", resp.StatusCode)
		}

		var payload struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return TokenData{}, fmt.Errorf("invalid Codex token refresh JSON: %w", err)
		}
		if strings.TrimSpace(payload.AccessToken) == "" {
			return TokenData{}, errors.New("Codex token refresh returned empty access_token")
		}
		newRefresh := payload.RefreshToken
		if strings.TrimSpace(newRefresh) == "" {
			newRefresh = refreshToken
		}
		claims := ParseIDTokenClaims(payload.IDToken)
		var acctIDPtr *string
		if claims.ChatGPTAccountID != "" {
			v := claims.ChatGPTAccountID
			acctIDPtr = &v
		}
		return TokenData{
			IDToken:      payload.IDToken,
			AccessToken:  payload.AccessToken,
			RefreshToken: newRefresh,
			AccountID:    acctIDPtr,
		}, nil
	}
	if lastErr != nil {
		return TokenData{}, lastErr
	}
	return TokenData{}, errors.New("Codex token refresh failed")
}

type PKCESession struct {
	AuthorizeURL string
	RedirectURI  string
	Wait         func(ctx context.Context) (TokenData, error)
	Close        func() error
}

func StartPKCELogin(cfg OAuthConfig) (*PKCESession, error) {
	issuer := strings.TrimRight(cfg.Issuer, "/")
	if issuer == "" {
		issuer = DefaultIssuer
	}
	clientID := cfg.ClientID
	if clientID == "" {
		clientID = DefaultClientID
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	verifier, challenge, err := generatePKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomURLSafe(24)
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", DefaultAuthPort))
	if err != nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", port)

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", "codex_cli_rs")
	authURL := issuer + "/oauth/authorize?" + q.Encode()

	type callbackResult struct {
		code string
		err  error
	}
	resCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			http.Error(w, "OAuth authorization failed: "+errParam, http.StatusBadRequest)
			select {
			case resCh <- callbackResult{err: fmt.Errorf("OAuth error: %s", errParam)}:
			default:
			}
			return
		}
		if r.URL.Query().Get("state") != state {
			http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
			select {
			case resCh <- callbackResult{err: errors.New("OAuth state mismatch")}:
			default:
			}
			return
		}
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if code == "" {
			http.Error(w, "Missing authorization code", http.StatusBadRequest)
			select {
			case resCh <- callbackResult{err: errors.New("missing authorization code")}:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body><h2>Codex sign-in complete</h2><p>You can close this tab and return to your terminal.</p></body></html>")
		select {
		case resCh <- callbackResult{code: code}:
		default:
		}
	})

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(listener)
	}()

	return &PKCESession{
		AuthorizeURL: authURL,
		RedirectURI:  redirectURI,
		Close: func() error {
			return srv.Close()
		},
		Wait: func(ctx context.Context) (TokenData, error) {
			defer func() { _ = srv.Close() }()
			select {
			case <-ctx.Done():
				return TokenData{}, ctx.Err()
			case cb := <-resCh:
				if cb.err != nil {
					return TokenData{}, cb.err
				}
				return exchangeAuthorizationCode(ctx, client, issuer, clientID, redirectURI, verifier, cb.code)
			}
		},
	}, nil
}

func exchangeAuthorizationCode(ctx context.Context, client *http.Client, issuer, clientID, redirectURI, verifier, code string) (TokenData, error) {
	exCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(exCtx, http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenData{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return TokenData{}, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenData{}, fmt.Errorf("Codex code exchange failed (HTTP %d)", resp.StatusCode)
	}
	var payload struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return TokenData{}, fmt.Errorf("invalid Codex token response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" || strings.TrimSpace(payload.RefreshToken) == "" {
		return TokenData{}, errors.New("Codex token response missing access or refresh token")
	}
	claims := ParseIDTokenClaims(payload.IDToken)
	var acctIDPtr *string
	if claims.ChatGPTAccountID != "" {
		v := claims.ChatGPTAccountID
		acctIDPtr = &v
	}
	return TokenData{
		IDToken:      payload.IDToken,
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		AccountID:    acctIDPtr,
	}, nil
}

func generatePKCE() (string, string, error) {
	verifier, err := randomURLSafe(48)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomURLSafe(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
