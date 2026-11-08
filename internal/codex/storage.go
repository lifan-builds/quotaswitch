package codex

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	AuthModeChatGPT = "chat_g_p_t"
	AuthModeAPIKey  = "api_key"
)

var (
	ErrNoLiveAuth    = errors.New("no live Codex auth.json found")
	ErrNoAccounts    = errors.New("no stored Codex accounts")
	ErrNotFound      = errors.New("matching Codex account not found")
	ErrAmbiguous     = errors.New("query matches multiple Codex accounts")
	ErrMissingTokens = errors.New("Codex account is missing OAuth tokens")
)

type AccountsStore struct {
	Version          uint32          `json:"version"`
	Accounts         []StoredAccount `json:"accounts"`
	ActiveAccountID  *string         `json:"active_account_id"`
	MaskedAccountIDs []string        `json:"masked_account_ids,omitempty"`
}

type StoredAccount struct {
	ID                    string     `json:"id"`
	Name                  string     `json:"name"`
	Email                 *string    `json:"email"`
	PlanType              *string    `json:"plan_type"`
	SubscriptionExpiresAt *time.Time `json:"subscription_expires_at,omitempty"`
	AuthMode              string     `json:"auth_mode"`
	AuthData              AuthData   `json:"auth_data"`
	CreatedAt             time.Time  `json:"created_at"`
	LastUsedAt            *time.Time `json:"last_used_at"`
}

func (a StoredAccount) DisplayName() string {
	if strings.TrimSpace(a.Name) != "" {
		if a.Email != nil && strings.TrimSpace(*a.Email) != "" && !strings.EqualFold(a.Name, *a.Email) {
			return fmt.Sprintf("%s (%s)", a.Name, *a.Email)
		}
		return a.Name
	}
	if a.Email != nil && strings.TrimSpace(*a.Email) != "" {
		return *a.Email
	}
	return a.ID
}

func (a StoredAccount) EmailString() string {
	if a.Email == nil {
		return ""
	}
	return strings.TrimSpace(*a.Email)
}

func (a StoredAccount) PlanString() string {
	if a.PlanType == nil || strings.TrimSpace(*a.PlanType) == "" {
		if a.AuthMode == AuthModeAPIKey {
			return "api_key"
		}
		return ""
	}
	return strings.TrimSpace(*a.PlanType)
}

func (a StoredAccount) WorkspaceLabel() string {
	if a.AuthData.AccountID != nil {
		ws := strings.TrimSpace(*a.AuthData.AccountID)
		if ws != "" {
			if len(ws) > 6 {
				ws = ws[len(ws)-6:]
			}
			return "ws-" + strings.ToLower(ws)
		}
	}
	return "primary"
}

type AuthData struct {
	Type         string  `json:"type"`
	Key          *string `json:"key,omitempty"`
	IDToken      string  `json:"id_token,omitempty"`
	AccessToken  string  `json:"access_token,omitempty"`
	RefreshToken string  `json:"refresh_token,omitempty"`
	AccountID    *string `json:"account_id,omitempty"`
}

type TokenData struct {
	IDToken      string  `json:"id_token"`
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	AccountID    *string `json:"account_id,omitempty"`
}

type AuthDotJson struct {
	OpenAIAPIKey *string    `json:"OPENAI_API_KEY,omitempty"`
	Tokens       *TokenData `json:"tokens,omitempty"`
	LastRefresh  *time.Time `json:"last_refresh,omitempty"`
}

type IDTokenClaims struct {
	Email                 string
	Sub                   string
	ChatGPTAccountID      string
	PlanType              string
	SubscriptionExpiresAt *time.Time
	ExpiresAt             *time.Time
}

type Store struct {
	SwitcherDir string
	CodexHome   string
}

func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	switcherDir := strings.TrimSpace(os.Getenv("CODEX_SWITCHER_HOME"))
	if switcherDir == "" {
		switcherDir = filepath.Join(home, ".codex-switcher")
	}
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return NewStore(switcherDir, codexHome), nil
}

func NewStore(switcherDir, codexHome string) *Store {
	return &Store{
		SwitcherDir: switcherDir,
		CodexHome:   codexHome,
	}
}

func (s *Store) AccountsPath() string {
	return filepath.Join(s.SwitcherDir, "accounts.json")
}

func (s *Store) LiveAuthPath() string {
	return filepath.Join(s.CodexHome, "auth.json")
}

func (s *Store) Exists() bool {
	if s == nil {
		return false
	}
	if _, err := os.Stat(s.AccountsPath()); err == nil {
		return true
	}
	if _, err := os.Stat(s.LiveAuthPath()); err == nil {
		return true
	}
	return false
}

func (s *Store) Load() (*AccountsStore, error) {
	raw, err := os.ReadFile(s.AccountsPath())
	if errors.Is(err, os.ErrNotExist) {
		return &AccountsStore{Version: 1, Accounts: []StoredAccount{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var data AccountsStore
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("invalid Codex accounts.json: %w", err)
	}
	if data.Version == 0 {
		data.Version = 1
	}
	if data.Accounts == nil {
		data.Accounts = []StoredAccount{}
	}
	return &data, nil
}

func (s *Store) Save(data *AccountsStore) error {
	if data.Version == 0 {
		data.Version = 1
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(s.AccountsPath(), encoded)
}

func (s *Store) ReadLiveAuth() (*AuthDotJson, error) {
	raw, err := os.ReadFile(s.LiveAuthPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoLiveAuth
	}
	if err != nil {
		return nil, err
	}
	var auth AuthDotJson
	if err := json.Unmarshal(raw, &auth); err != nil {
		return nil, fmt.Errorf("invalid Codex auth.json: %w", err)
	}
	return &auth, nil
}

func (s *Store) WriteLiveAuth(account StoredAccount, now time.Time) error {
	var doc AuthDotJson
	switch account.AuthMode {
	case AuthModeAPIKey:
		if account.AuthData.Key == nil || strings.TrimSpace(*account.AuthData.Key) == "" {
			return errors.New("Codex API key account has an empty key")
		}
		key := *account.AuthData.Key
		doc.OpenAIAPIKey = &key
	default:
		if strings.TrimSpace(account.AuthData.AccessToken) == "" || strings.TrimSpace(account.AuthData.RefreshToken) == "" {
			return ErrMissingTokens
		}
		ts := now.UTC()
		doc.Tokens = &TokenData{
			IDToken:      account.AuthData.IDToken,
			AccessToken:  account.AuthData.AccessToken,
			RefreshToken: account.AuthData.RefreshToken,
			AccountID:    account.AuthData.AccountID,
		}
		doc.LastRefresh = &ts
	}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(s.LiveAuthPath(), encoded)
}

// SyncActiveFromLiveAuth reconciles single-use refresh tokens rotated by Codex
// in ~/.codex/auth.json back into ~/.codex-switcher/accounts.json.
func (s *Store) SyncActiveFromLiveAuth(data *AccountsStore, now time.Time) (bool, error) {
	live, err := s.ReadLiveAuth()
	if err != nil {
		if errors.Is(err, ErrNoLiveAuth) {
			return false, nil
		}
		return false, err
	}
	if live.Tokens == nil || strings.TrimSpace(live.Tokens.AccessToken) == "" {
		return false, nil
	}
	liveClaims := ParseIDTokenClaims(live.Tokens.IDToken)
	liveAcctID := ""
	if live.Tokens.AccountID != nil {
		liveAcctID = strings.TrimSpace(*live.Tokens.AccountID)
	}
	if liveAcctID == "" {
		liveAcctID = liveClaims.ChatGPTAccountID
	}

	idx := matchStoredAccountIndex(data, liveClaims, liveAcctID)
	if idx < 0 {
		return false, nil
	}

	changed := false
	acct := &data.Accounts[idx]
	if acct.AuthData.AccessToken != live.Tokens.AccessToken ||
		acct.AuthData.RefreshToken != live.Tokens.RefreshToken ||
		(live.Tokens.IDToken != "" && acct.AuthData.IDToken != live.Tokens.IDToken) {
		acct.AuthData.AccessToken = live.Tokens.AccessToken
		if live.Tokens.RefreshToken != "" {
			acct.AuthData.RefreshToken = live.Tokens.RefreshToken
		}
		if live.Tokens.IDToken != "" {
			acct.AuthData.IDToken = live.Tokens.IDToken
		}
		if liveAcctID != "" {
			val := liveAcctID
			acct.AuthData.AccountID = &val
		}
		if liveClaims.Email != "" {
			val := liveClaims.Email
			acct.Email = &val
		}
		if liveClaims.PlanType != "" {
			val := liveClaims.PlanType
			acct.PlanType = &val
		}
		if liveClaims.SubscriptionExpiresAt != nil {
			acct.SubscriptionExpiresAt = liveClaims.SubscriptionExpiresAt
		}
		changed = true
	}

	if data.ActiveAccountID == nil || *data.ActiveAccountID != acct.ID {
		id := acct.ID
		data.ActiveAccountID = &id
		ts := now.UTC()
		acct.LastUsedAt = &ts
		changed = true
	}

	if changed {
		if err := s.Save(data); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func matchStoredAccountIndex(data *AccountsStore, claims IDTokenClaims, accountID string) int {
	if data.ActiveAccountID != nil {
		for i := range data.Accounts {
			a := data.Accounts[i]
			if a.ID == *data.ActiveAccountID && sameIdentity(a, claims, accountID) {
				return i
			}
		}
	}
	for i := range data.Accounts {
		if sameIdentity(data.Accounts[i], claims, accountID) {
			return i
		}
	}
	return -1
}

func sameIdentity(a StoredAccount, claims IDTokenClaims, accountID string) bool {
	if a.AuthMode != AuthModeChatGPT {
		return false
	}
	storedClaims := ParseIDTokenClaims(a.AuthData.IDToken)
	storedAcctID := ""
	if a.AuthData.AccountID != nil {
		storedAcctID = strings.TrimSpace(*a.AuthData.AccountID)
	}
	if storedAcctID == "" {
		storedAcctID = storedClaims.ChatGPTAccountID
	}
	if accountID != "" && storedAcctID != "" && accountID != storedAcctID {
		return false
	}
	if claims.Sub != "" && storedClaims.Sub != "" {
		return claims.Sub == storedClaims.Sub
	}
	if claims.Email != "" && a.EmailString() != "" {
		return strings.EqualFold(claims.Email, a.EmailString())
	}
	return accountID != "" && accountID == storedAcctID
}

func (s *Store) UpsertTokens(tokens TokenData, alias string, activate bool, now time.Time) (*StoredAccount, bool, error) {
	data, err := s.Load()
	if err != nil {
		return nil, false, err
	}
	claims := ParseIDTokenClaims(tokens.IDToken)
	acctID := ""
	if tokens.AccountID != nil && strings.TrimSpace(*tokens.AccountID) != "" {
		acctID = strings.TrimSpace(*tokens.AccountID)
	} else if claims.ChatGPTAccountID != "" {
		acctID = claims.ChatGPTAccountID
	}
	var acctIDPtr *string
	if acctID != "" {
		v := acctID
		acctIDPtr = &v
	}

	idx := matchStoredAccountIndex(data, claims, acctID)
	ts := now.UTC()
	added := false
	if idx >= 0 {
		acct := &data.Accounts[idx]
		acct.AuthMode = AuthModeChatGPT
		acct.AuthData = AuthData{
			Type:         AuthModeChatGPT,
			IDToken:      tokens.IDToken,
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			AccountID:    acctIDPtr,
		}
		if strings.TrimSpace(alias) != "" {
			acct.Name = uniqueName(data.Accounts, strings.TrimSpace(alias), acct.ID)
		}
		if claims.Email != "" {
			v := claims.Email
			acct.Email = &v
		}
		if claims.PlanType != "" {
			v := claims.PlanType
			acct.PlanType = &v
		}
		if claims.SubscriptionExpiresAt != nil {
			acct.SubscriptionExpiresAt = claims.SubscriptionExpiresAt
		}
		if activate {
			id := acct.ID
			data.ActiveAccountID = &id
			acct.LastUsedAt = &ts
		}
	} else {
		added = true
		id := newUUID()
		baseName := strings.TrimSpace(alias)
		if baseName == "" {
			baseName = claims.Email
		}
		if baseName == "" && acctID != "" {
			suffix := acctID
			if len(suffix) > 8 {
				suffix = suffix[len(suffix)-8:]
			}
			baseName = fmt.Sprintf("Codex (%s)", suffix)
		}
		if baseName == "" {
			baseName = "Codex account"
		}
		var emailPtr, planPtr *string
		if claims.Email != "" {
			v := claims.Email
			emailPtr = &v
		}
		if claims.PlanType != "" {
			v := claims.PlanType
			planPtr = &v
		}
		var lastUsed *time.Time
		if activate {
			lastUsed = &ts
		}
		newAcct := StoredAccount{
			ID:                    id,
			Name:                  uniqueName(data.Accounts, baseName, id),
			Email:                 emailPtr,
			PlanType:              planPtr,
			SubscriptionExpiresAt: claims.SubscriptionExpiresAt,
			AuthMode:              AuthModeChatGPT,
			AuthData: AuthData{
				Type:         AuthModeChatGPT,
				IDToken:      tokens.IDToken,
				AccessToken:  tokens.AccessToken,
				RefreshToken: tokens.RefreshToken,
				AccountID:    acctIDPtr,
			},
			CreatedAt:  ts,
			LastUsedAt: lastUsed,
		}
		data.Accounts = append(data.Accounts, newAcct)
		idx = len(data.Accounts) - 1
		if activate {
			activeID := newAcct.ID
			data.ActiveAccountID = &activeID
		}
	}

	if err := s.Save(data); err != nil {
		return nil, false, err
	}
	if activate {
		if err := s.WriteLiveAuth(data.Accounts[idx], now); err != nil {
			return nil, false, err
		}
	}
	result := data.Accounts[idx]
	return &result, added, nil
}

func (s *Store) AddFromLiveAuth(alias string, now time.Time) (*StoredAccount, bool, error) {
	live, err := s.ReadLiveAuth()
	if err != nil {
		return nil, false, err
	}
	if live.Tokens != nil && strings.TrimSpace(live.Tokens.AccessToken) != "" {
		return s.UpsertTokens(*live.Tokens, alias, true, now)
	}
	if live.OpenAIAPIKey != nil && strings.TrimSpace(*live.OpenAIAPIKey) != "" {
		data, err := s.Load()
		if err != nil {
			return nil, false, err
		}
		key := strings.TrimSpace(*live.OpenAIAPIKey)
		for i := range data.Accounts {
			if data.Accounts[i].AuthMode == AuthModeAPIKey && data.Accounts[i].AuthData.Key != nil && *data.Accounts[i].AuthData.Key == key {
				id := data.Accounts[i].ID
				data.ActiveAccountID = &id
				if strings.TrimSpace(alias) != "" {
					data.Accounts[i].Name = uniqueName(data.Accounts, strings.TrimSpace(alias), id)
				}
				if err := s.Save(data); err != nil {
					return nil, false, err
				}
				acct := data.Accounts[i]
				return &acct, false, nil
			}
		}
		ts := now.UTC()
		id := newUUID()
		name := strings.TrimSpace(alias)
		if name == "" {
			name = "API Key account"
		}
		plan := "api_key"
		newAcct := StoredAccount{
			ID:       id,
			Name:     uniqueName(data.Accounts, name, id),
			PlanType: &plan,
			AuthMode: AuthModeAPIKey,
			AuthData: AuthData{
				Type: AuthModeAPIKey,
				Key:  &key,
			},
			CreatedAt:  ts,
			LastUsedAt: &ts,
		}
		data.Accounts = append(data.Accounts, newAcct)
		data.ActiveAccountID = &id
		if err := s.Save(data); err != nil {
			return nil, false, err
		}
		return &newAcct, true, nil
	}
	return nil, false, ErrNoLiveAuth
}

func Resolve(data *AccountsStore, query string) (*StoredAccount, error) {
	if data == nil || len(data.Accounts) == 0 {
		return nil, ErrNoAccounts
	}
	q := strings.TrimSpace(query)
	if q == "-" {
		active := ""
		if data.ActiveAccountID != nil {
			active = *data.ActiveAccountID
		}
		var candidates []StoredAccount
		for _, a := range data.Accounts {
			if a.ID != active && a.LastUsedAt != nil {
				candidates = append(candidates, a)
			}
		}
		if len(candidates) == 0 {
			for _, a := range data.Accounts {
				if a.ID != active {
					cp := a
					return &cp, nil
				}
			}
			return nil, ErrNotFound
		}
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].LastUsedAt.After(*candidates[j].LastUsedAt)
		})
		cp := candidates[0]
		return &cp, nil
	}

	if num, err := strconv.Atoi(q); err == nil && num >= 1 && num <= len(data.Accounts) {
		cp := data.Accounts[num-1]
		return &cp, nil
	}

	for _, a := range data.Accounts {
		if a.ID == q || strings.EqualFold(a.Name, q) {
			cp := a
			return &cp, nil
		}
		if a.AuthData.AccountID != nil && strings.EqualFold(strings.TrimSpace(*a.AuthData.AccountID), q) {
			cp := a
			return &cp, nil
		}
	}

	var exactWorkspace []StoredAccount
	for _, a := range data.Accounts {
		if strings.EqualFold(a.WorkspaceLabel(), q) {
			exactWorkspace = append(exactWorkspace, a)
		}
	}
	if len(exactWorkspace) == 1 {
		cp := exactWorkspace[0]
		return &cp, nil
	}
	if len(exactWorkspace) > 1 {
		return nil, ErrAmbiguous
	}

	var exactEmail []StoredAccount
	for _, a := range data.Accounts {
		if strings.EqualFold(a.EmailString(), q) {
			exactEmail = append(exactEmail, a)
		}
	}
	if len(exactEmail) == 1 {
		cp := exactEmail[0]
		return &cp, nil
	}
	if len(exactEmail) > 1 {
		return nil, ErrAmbiguous
	}

	lower := strings.ToLower(q)
	var partial []StoredAccount
	for _, a := range data.Accounts {
		if strings.HasPrefix(strings.ToLower(a.ID), lower) ||
			strings.Contains(strings.ToLower(a.Name), lower) ||
			strings.Contains(strings.ToLower(a.EmailString()), lower) {
			partial = append(partial, a)
		}
	}
	if len(partial) == 1 {
		cp := partial[0]
		return &cp, nil
	}
	if len(partial) > 1 {
		return nil, ErrAmbiguous
	}
	return nil, ErrNotFound
}

func ParseIDTokenClaims(jwt string) IDTokenClaims {
	var out IDTokenClaims
	parts := strings.Split(strings.TrimSpace(jwt), ".")
	if len(parts) < 2 {
		return out
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return out
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return out
	}
	if v, ok := doc["email"].(string); ok {
		out.Email = strings.TrimSpace(v)
	}
	if v, ok := doc["sub"].(string); ok {
		out.Sub = strings.TrimSpace(v)
	}
	if exp, ok := asInt64(doc["exp"]); ok && exp > 0 {
		t := time.Unix(exp, 0).UTC()
		out.ExpiresAt = &t
	}
	if authObj, ok := doc["https://api.openai.com/auth"].(map[string]any); ok {
		if v, ok := authObj["chatgpt_account_id"].(string); ok {
			out.ChatGPTAccountID = strings.TrimSpace(v)
		}
		if v, ok := authObj["chatgpt_plan_type"].(string); ok {
			out.PlanType = strings.TrimSpace(v)
		}
		if v, ok := authObj["chatgpt_subscription_active_until"].(string); ok && strings.TrimSpace(v) != "" {
			if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
				u := parsed.UTC()
				out.SubscriptionExpiresAt = &u
			}
		} else if ts, ok := asInt64(authObj["chatgpt_subscription_active_until"]); ok && ts > 0 {
			u := time.Unix(ts, 0).UTC()
			out.SubscriptionExpiresAt = &u
		}
	}
	return out
}

func TokenExpiresWithin(jwt string, now time.Time, skew time.Duration) bool {
	claims := ParseIDTokenClaims(jwt)
	if claims.ExpiresAt == nil {
		return false
	}
	return !claims.ExpiresAt.After(now.Add(skew))
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		val, err := n.Int64()
		return val, err == nil
	default:
		return 0, false
	}
}

func uniqueName(accounts []StoredAccount, desired, selfID string) string {
	taken := func(candidate string) bool {
		for _, a := range accounts {
			if a.ID != selfID && strings.EqualFold(strings.TrimSpace(a.Name), candidate) {
				return true
			}
		}
		return false
	}
	if !taken(desired) {
		return desired
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s (%d)", desired, i)
		if !taken(candidate) {
			return candidate
		}
	}
	return desired + "-" + selfID[:6]
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("acct-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

func writeFileAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	tmpPath := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%s", filepath.Base(path), hex.EncodeToString(suffix[:])))
	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Chmod(path, 0o600)
}
