package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/lifan-builds/quotaswitch/internal/agy"
	"github.com/lifan-builds/quotaswitch/internal/codex"
	"github.com/lifan-builds/quotaswitch/internal/registry"
	"github.com/lifan-builds/quotaswitch/internal/usage"
)

const defaultCooldown = 2 * time.Minute

type CodexCandidate struct {
	ID          string
	DisplayName string
	AuthMode    string
	AccessToken string
	APIKey      string
	AccountID   string
	Account     codex.StoredAccount
}

type AgyCandidate struct {
	ID          string
	DisplayName string
	Token       *agy.TokenFile
}

type Pool struct {
	CodexStore *codex.Store
	CodexOAuth codex.OAuthConfig
	AgyStore   *registry.Store
	AgyStorage *agy.Storage
	AgyUsage   *usage.Client
	Now        func() time.Time
	// CodexRunning reports whether Codex is running; the active account's single-use
	// refresh token must not be rotated while Codex holds it.
	CodexRunning func() bool

	mu             sync.Mutex
	preferredCodex string
	preferredAgy   string
	cooldownUntil  map[string]time.Time
	projectCache   map[string]string
}

func NewPool(codexStore *codex.Store, agyStore *registry.Store, agyStorage *agy.Storage, agyUsage *usage.Client) *Pool {
	if agyUsage == nil {
		agyUsage = usage.NewClient("gateway")
	}
	return &Pool{
		CodexStore:    codexStore,
		CodexOAuth:    codex.DefaultOAuthConfig(),
		AgyStore:      agyStore,
		AgyStorage:    agyStorage,
		AgyUsage:      agyUsage,
		Now:           time.Now,
		cooldownUntil: make(map[string]time.Time),
		projectCache:  make(map[string]string),
	}
}

func (p *Pool) now() time.Time {
	if p != nil && p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Pool) cooldownKey(provider, id string) string {
	return provider + ":" + id
}

func (p *Pool) MarkCooldown(provider, id string, d time.Duration) {
	if id == "" {
		return
	}
	if d <= 0 {
		d = defaultCooldown
	}
	p.mu.Lock()
	if p.cooldownUntil == nil {
		p.cooldownUntil = make(map[string]time.Time)
	}
	p.cooldownUntil[p.cooldownKey(provider, id)] = p.now().Add(d)
	p.mu.Unlock()
}

func (p *Pool) MarkHealthy(provider, id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	delete(p.cooldownUntil, p.cooldownKey(provider, id))
	switch provider {
	case BackendCodex:
		p.preferredCodex = id
	case BackendAgy:
		p.preferredAgy = id
	}
	p.mu.Unlock()
}

func (p *Pool) isCoolingDown(provider, id string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.cooldownUntil[p.cooldownKey(provider, id)]
	if !ok {
		return false
	}
	if !until.After(now) {
		delete(p.cooldownUntil, p.cooldownKey(provider, id))
		return false
	}
	return true
}

// CodexCandidates returns ordered Codex accounts ready for request execution.
func (p *Pool) CodexCandidates() ([]CodexCandidate, error) {
	if p.CodexStore == nil {
		return nil, errors.New("Codex account store is not configured")
	}
	data, err := p.CodexStore.Load()
	if err != nil {
		return nil, err
	}
	now := p.now()
	_, _ = p.CodexStore.SyncActiveFromLiveAuth(data, now)
	if len(data.Accounts) == 0 {
		if _, _, addErr := p.CodexStore.AddFromLiveAuth("", now); addErr == nil {
			data, _ = p.CodexStore.Load()
		}
	}
	if len(data.Accounts) == 0 {
		return nil, codex.ErrNoAccounts
	}

	p.mu.Lock()
	preferred := p.preferredCodex
	p.mu.Unlock()
	if preferred == "" && data.ActiveAccountID != nil {
		preferred = *data.ActiveAccountID
	}

	startIdx := 0
	if preferred != "" {
		for i, a := range data.Accounts {
			if a.ID == preferred {
				startIdx = i
				break
			}
		}
	}

	ready := make([]CodexCandidate, 0, len(data.Accounts))
	cooling := make([]CodexCandidate, 0, len(data.Accounts))
	for offset := 0; offset < len(data.Accounts); offset++ {
		acct := data.Accounts[(startIdx+offset)%len(data.Accounts)]
		cand := CodexCandidate{
			ID:          acct.ID,
			DisplayName: acct.DisplayName(),
			AuthMode:    acct.AuthMode,
			AccessToken: strings.TrimSpace(acct.AuthData.AccessToken),
			Account:     acct,
		}
		if acct.AuthData.Key != nil {
			cand.APIKey = strings.TrimSpace(*acct.AuthData.Key)
		}
		if acct.AuthData.AccountID != nil {
			cand.AccountID = strings.TrimSpace(*acct.AuthData.AccountID)
		}
		if cand.AccountID == "" && acct.AuthData.IDToken != "" {
			claims := codex.ParseIDTokenClaims(acct.AuthData.IDToken)
			cand.AccountID = strings.TrimSpace(claims.ChatGPTAccountID)
		}
		if p.isCoolingDown(BackendCodex, acct.ID, now) {
			cooling = append(cooling, cand)
		} else {
			ready = append(ready, cand)
		}
	}
	return append(ready, cooling...), nil
}

// EnsureCodexToken refreshes a Codex candidate's OAuth token if it is expired or Force is true.
func (p *Pool) EnsureCodexToken(ctx context.Context, cand *CodexCandidate, force bool) error {
	if cand == nil {
		return errors.New("nil Codex candidate")
	}
	if cand.AuthMode == codex.AuthModeAPIKey {
		if cand.APIKey == "" {
			return codex.ErrMissingTokens
		}
		return nil
	}
	now := p.now()
	needsRefresh := force || codex.NeedsRefresh(cand.Account.AuthData.IDToken, cand.AccessToken, now)
	if !needsRefresh && cand.AccessToken != "" {
		return nil
	}
	if p.CodexRunning != nil && p.CodexStore != nil {
		if data, err := p.CodexStore.Load(); err == nil && data.ActiveAccountID != nil && *data.ActiveAccountID == cand.ID && p.CodexRunning() {
			if cand.AccessToken != "" && !force {
				return nil
			}
			return errors.New("active Codex account is in use by Codex; skipping token refresh")
		}
	}
	refreshTok := strings.TrimSpace(cand.Account.AuthData.RefreshToken)
	if refreshTok == "" {
		if cand.AccessToken != "" && !force {
			return nil
		}
		return codex.ErrMissingTokens
	}
	cfg := p.CodexOAuth
	if cfg.Issuer == "" {
		cfg = codex.DefaultOAuthConfig()
	}
	refreshed, err := codex.RefreshTokens(ctx, cfg, refreshTok)
	if err != nil {
		return err
	}
	cand.AccessToken = strings.TrimSpace(refreshed.AccessToken)
	cand.Account.AuthData.AccessToken = cand.AccessToken
	cand.Account.AuthData.RefreshToken = refreshed.RefreshToken
	if refreshed.IDToken != "" {
		cand.Account.AuthData.IDToken = refreshed.IDToken
	}
	if refreshed.AccountID != nil && strings.TrimSpace(*refreshed.AccountID) != "" {
		cand.Account.AuthData.AccountID = refreshed.AccountID
		cand.AccountID = strings.TrimSpace(*refreshed.AccountID)
	}
	// Persist updated tokens back to accounts.json
	if p.CodexStore != nil {
		if data, loadErr := p.CodexStore.Load(); loadErr == nil {
			for i := range data.Accounts {
				if data.Accounts[i].ID == cand.ID {
					data.Accounts[i].AuthData = cand.Account.AuthData
					break
				}
			}
			_ = p.CodexStore.Save(data)
			if data.ActiveAccountID != nil && *data.ActiveAccountID == cand.ID {
				_ = p.CodexStore.WriteLiveAuth(cand.Account, now)
			}
		}
	}
	return nil
}

// AgyCandidates returns ordered Antigravity accounts ready for request execution.
func (p *Pool) AgyCandidates() ([]AgyCandidate, error) {
	now := p.now()
	var reg *registry.Registry
	if p.AgyStore != nil {
		if loaded, err := p.AgyStore.Load(); err == nil {
			reg = loaded
		}
	}

	p.mu.Lock()
	preferred := p.preferredAgy
	p.mu.Unlock()
	if preferred == "" && reg != nil {
		preferred = reg.Active
	}

	var all []AgyCandidate
	if reg != nil && p.AgyStore != nil && len(reg.Accounts) > 0 {
		startIdx := 0
		if preferred != "" {
			for i, a := range reg.Accounts {
				if a.ID == preferred {
					startIdx = i
					break
				}
			}
		}
		for offset := 0; offset < len(reg.Accounts); offset++ {
			acct := reg.Accounts[(startIdx+offset)%len(reg.Accounts)]
			raw, err := p.AgyStore.LoadToken(acct.ID)
			if err != nil {
				continue
			}
			tf, err := agy.ParseTokenFile(raw)
			if err != nil {
				continue
			}
			all = append(all, AgyCandidate{
				ID:          acct.ID,
				DisplayName: acct.DisplayName(),
				Token:       tf,
			})
		}
	}

	if len(all) == 0 && p.AgyStorage != nil {
		if res, err := p.AgyStorage.Read(); err == nil {
			if tf, pErr := agy.ParseTokenFile(res.Raw); pErr == nil && tf != nil {
				all = append(all, AgyCandidate{
					ID:          "live",
					DisplayName: tf.Email(),
					Token:       tf,
				})
			}
		}
	}

	if len(all) == 0 {
		return nil, errors.New("no stored Antigravity accounts available")
	}

	ready := make([]AgyCandidate, 0, len(all))
	cooling := make([]AgyCandidate, 0, len(all))
	for _, cand := range all {
		if p.isCoolingDown(BackendAgy, cand.ID, now) {
			cooling = append(cooling, cand)
		} else {
			ready = append(ready, cand)
		}
	}
	return append(ready, cooling...), nil
}

// ResolveAgySession returns a valid OAuth access token and CloudCode project ID for an Antigravity candidate.
func (p *Pool) ResolveAgySession(ctx context.Context, cand *AgyCandidate, forceRefresh bool) (string, string, error) {
	if cand == nil || cand.Token == nil {
		return "", "", usage.ErrMissingAuth
	}
	if p.AgyUsage == nil {
		p.AgyUsage = usage.NewClient("gateway")
	}
	if forceRefresh {
		p.AgyUsage.EvictAccessToken(cand.Token)
	}

	p.mu.Lock()
	cachedProj := p.projectCache[cand.ID]
	p.mu.Unlock()

	if cachedProj != "" && !forceRefresh {
		tok, err := p.AgyUsage.AccessToken(ctx, cand.Token)
		if err == nil && tok != "" {
			return tok, cachedProj, nil
		}
	}

	tok, proj, err := p.AgyUsage.ResolveProject(ctx, cand.Token)
	if err != nil {
		return "", "", err
	}
	if proj != "" {
		p.mu.Lock()
		if p.projectCache == nil {
			p.projectCache = make(map[string]string)
		}
		p.projectCache[cand.ID] = proj
		p.mu.Unlock()
	}
	return tok, proj, nil
}

// Summary returns the count of pooled Codex and Antigravity accounts.
func (p *Pool) Summary() (codexCount int, agyCount int) {
	if p.CodexStore != nil {
		if data, err := p.CodexStore.Load(); err == nil {
			codexCount = len(data.Accounts)
		}
	}
	if p.AgyStore != nil {
		if reg, err := p.AgyStore.Load(); err == nil {
			agyCount = len(reg.Accounts)
		}
	}
	return codexCount, agyCount
}
