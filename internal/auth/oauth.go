package auth

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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sam/gemini-remote-linux/internal/config"
)

const (
	AuthURL  = "https://accounts.google.com/o/oauth2/auth"
	TokenURL = "https://www.googleapis.com/oauth2/v4/token"
)

func resolveCredentials() (string, string, error) {
	id := strings.TrimSpace(os.Getenv("GEMINI_REMOTE_CLIENT_ID"))
	secret := strings.TrimSpace(os.Getenv("GEMINI_REMOTE_CLIENT_SECRET"))
	if id == "" || secret == "" {
		if cfg, err := config.LoadOrInit(); err == nil {
			if id == "" {
				id = strings.TrimSpace(cfg.OAuthClientID)
			}
			if secret == "" {
				secret = strings.TrimSpace(cfg.OAuthClientSecret)
			}
		}
	}
	if id == "" || secret == "" {
		return "", "", errors.New("OAuth Client ID/Secret이 설정되지 않았습니다 ('gemini-remote extract-keys <Gemini.app 경로>'를 실행하거나 ~/.config/gemini-remote/config.json에 oauth_client_id, oauth_client_secret을 설정하세요)")
	}
	return id, secret, nil
}

var clientIDRegex = regexp.MustCompile(`[0-9]{10,15}-[a-z0-9]{20,40}\.apps\.googleusercontent\.com`)

// ExtractCredentialsFromApp은 macOS Gemini.app 번들 또는 실행 바이너리에서
// OAuth 2.0 Client ID와 마스킹된 Client Secret을 추출한다.
func ExtractCredentialsFromApp(appOrBinPath string) (string, string, error) {
	target := appOrBinPath
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		cand := filepath.Join(target, "Contents", "MacOS", "Gemini")
		if _, err := os.Stat(cand); err == nil {
			target = cand
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", "", fmt.Errorf("바이너리 읽기 실패 (%s): %w", target, err)
	}

	clientID := string(clientIDRegex.Find(data))
	if clientID == "" {
		return "", "", errors.New("바이너리에서 Google OAuth Client ID(*.apps.googleusercontent.com)를 찾지 못했습니다")
	}

	// "GOCSPX-" (7바이트) + 28바이트 영숫자/특수문자 = 총 35바이트 시퀀스 스캔
	prefix := []byte("GOCSPX-")
	const secretLen = 35
	var clientSecret string

	for i := 0; i+secretLen <= len(data); i++ {
		key := data[i] ^ prefix[0]
		match := true
		for j := 1; j < len(prefix); j++ {
			if (data[i+j] ^ key) != prefix[j] {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		buf := make([]byte, secretLen)
		valid := true
		for j := 0; j < secretLen; j++ {
			c := data[i+j] ^ key
			if j >= len(prefix) && !isSecretChar(c) {
				valid = false
				break
			}
			buf[j] = c
		}
		if valid {
			clientSecret = string(buf)
			break
		}
	}

	if clientSecret == "" {
		return "", "", errors.New("바이너리에서 Google OAuth Client Secret을 찾지 못했습니다")
	}
	return clientID, clientSecret, nil
}

func isSecretChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}

// Scopes는 macOS Gemini.app이 요청하는 18개 OAuth 스코프 전체다.
var Scopes = []string{
	"openid",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/assistant",
	"https://www.googleapis.com/auth/client_channel",
	"https://www.googleapis.com/auth/gemini",
	"https://www.googleapis.com/auth/calendar",
	"https://www.googleapis.com/auth/drive",
	"https://www.googleapis.com/auth/photos.image.readonly",
	"https://www.googleapis.com/auth/photospicker.mediaitems.readonly",
	"https://www.googleapis.com/auth/oauth_integrations",
	"https://www.googleapis.com/auth/experimentsandconfigs",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/supportcontent",
	"https://www.googleapis.com/auth/mobile_user_preferences",
	"https://www.googleapis.com/auth/notifications",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/aicode",
}

// TokenData는 디스크에 저장되는 OAuth2 토큰 구조체다.
type TokenData struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
	Scope        string    `json:"scope,omitempty"`
}

// Manager는 OAuth 토큰 로드, 자동 갱신, 인터랙티브 로그인을 담당한다.
type Manager struct {
	mu         sync.Mutex
	token      *TokenData
	httpClient *http.Client
}

func NewManager() *Manager {
	return &Manager{
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

// LoadFromDisk는 저장된 토큰을 읽는다.
func (m *Manager) LoadFromDisk() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := config.TokenPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var td TokenData
	if err := json.Unmarshal(data, &td); err != nil {
		return err
	}
	if td.RefreshToken == "" && td.AccessToken == "" {
		return errors.New("empty token file")
	}
	m.token = &td
	return nil
}

func (m *Manager) saveLocked() error {
	if m.token == nil {
		return nil
	}
	p, err := config.TokenPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.token, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// AccessToken은 유효한 Access Token을 반환하며 만료 임박 시 Refresh Token으로 자동 갱신한다.
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.token == nil {
		if err := m.loadUnlocked(); err != nil {
			return "", fmt.Errorf("저장된 인증 토큰이 없습니다 ('gemini-remote login'을 먼저 실행하세요): %w", err)
		}
	}

	if m.token.AccessToken != "" && time.Until(m.token.Expiry) > 2*time.Minute {
		return m.token.AccessToken, nil
	}
	if m.token.RefreshToken == "" {
		return "", errors.New("refresh_token이 없어 토큰을 갱신할 수 없습니다 ('gemini-remote login'을 실행하세요)")
	}
	if err := m.refreshLocked(ctx); err != nil {
		return "", err
	}
	return m.token.AccessToken, nil
}

func (m *Manager) loadUnlocked() error {
	p, err := config.TokenPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var td TokenData
	if err := json.Unmarshal(data, &td); err != nil {
		return err
	}
	m.token = &td
	return nil
}

func (m *Manager) refreshLocked(ctx context.Context) error {
	clientID, clientSecret, err := resolveCredentials()
	if err != nil {
		return err
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {m.token.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OAuth 토큰 갱신 실패 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	m.token.AccessToken = raw.AccessToken
	if raw.RefreshToken != "" {
		m.token.RefreshToken = raw.RefreshToken
	}
	if raw.TokenType != "" {
		m.token.TokenType = raw.TokenType
	}
	if raw.Scope != "" {
		m.token.Scope = raw.Scope
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 3600
	}
	m.token.Expiry = time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	return m.saveLocked()
}

// LoginInteractive는 로컬 루프백 서버(127.0.0.1:<port>)와 PKCE를 사용해 브라우저 OAuth 로그인을 수행한다.
func (m *Manager) LoginInteractive(ctx context.Context, openBrowser bool) error {
	clientID, _, err := resolveCredentials()
	if err != nil {
		return err
	}
	verifier, challenge, err := generatePKCE()
	if err != nil {
		return err
	}
	state, err := randomURLSafe(24)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("로컬 루프백 리스너 생성 실패: %w", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d", port)

	params := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(Scopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	authURL := AuthURL + "?" + params.Encode()

	fmt.Printf("아래 URL을 브라우저에서 열어 Google 계정으로 로그인하세요:\n\n%s\n\n", authURL)
	if openBrowser {
		_ = exec.Command("xdg-open", authURL).Start()
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errStr := q.Get("error"); errStr != "" {
			http.Error(w, "OAuth 인증 실패: "+errStr, http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("OAuth 오류: %s", errStr):
			default:
			}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state 불일치", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "code 파라미터 없음", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body><h2>Gemini Remote Linux 인증이 완료되었습니다. 이 창을 닫으셔도 됩니다.</h2></body></html>")
		select {
		case codeCh <- code:
		default:
		}
	})

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(ln)
	}()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	var code string
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	case code = <-codeCh:
	}

	return m.exchangeCode(ctx, code, verifier, redirectURI)
}

func (m *Manager) exchangeCode(ctx context.Context, code, verifier, redirectURI string) error {
	clientID, clientSecret, err := resolveCredentials()
	if err != nil {
		return err
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("인증 코드 교환 실패 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 3600
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.token = &TokenData{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		TokenType:    raw.TokenType,
		Expiry:       time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
		Scope:        raw.Scope,
	}
	return m.saveLocked()
}

// ImportToken은 외부에서 발급받은 Refresh Token(1//...) 또는 Access Token(ya29...)을 등록한다.
func (m *Manager) ImportToken(ctx context.Context, rawToken string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasPrefix(rawToken, "1//") {
		m.token = &TokenData{RefreshToken: rawToken}
		return m.refreshLocked(ctx)
	}
	m.token = &TokenData{
		AccessToken: rawToken,
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(55 * time.Minute),
	}
	return m.saveLocked()
}

func generatePKCE() (verifier, challenge string, err error) {
	verifier, err = randomURLSafe(48)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
