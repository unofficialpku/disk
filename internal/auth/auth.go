// Package auth follows the official web login flow with a private in-memory
// cookie jar. Callers can persist the renewable session separately from passwords.
package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/unofficialpku/disk/internal/diagnostic"

	"golang.org/x/net/html"
)

type Challenge struct {
	Kind  string
	Image []byte
}
type ChallengeFunc func(context.Context, Challenge) (string, error)
type Options struct {
	Trace       *diagnostic.Logger
	Server      string
	IAAA        string
	Challenge   ChallengeFunc
	OnState     func(State) error
	RefreshLock func(context.Context) (io.Closer, error)
	LoadState   func() (State, error)
}
type Credentials struct {
	Username string
	Password []byte
	OTP      string
	SMS      string
	Captcha  string
}

const DefaultServer = "https://disk.pku.edu.cn"

var ErrSessionExpired = errors.New("session expired; log in again with username and password")

type HTTPError struct {
	Status int
	Method string
	Path   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("authentication %s %s: HTTP %d", e.Method, e.Path, e.Status)
}

type Session struct {
	trace       *diagnostic.Logger
	mu          sync.Mutex
	base        *url.URL
	iaaa        *url.URL
	http        *http.Client
	token       string
	challenge   ChallengeFunc
	onState     func(State) error
	refreshLock func(context.Context) (io.Closer, error)
	loadState   func() (State, error)
}

func New(opts Options) (*Session, error) {
	if opts.Server == "" {
		opts.Server = DefaultServer
	}
	if opts.IAAA == "" {
		opts.IAAA = "https://iaaa.pku.edu.cn"
	}
	base, err := validatedURL(opts.Server)
	if err != nil {
		return nil, err
	}
	iaaa, err := validatedURL(opts.IAAA)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	s := &Session{base: base, iaaa: iaaa, challenge: opts.Challenge, onState: opts.OnState, refreshLock: opts.RefreshLock, loadState: opts.LoadState, trace: opts.Trace}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	s.http = &http.Client{Transport: opts.Trace.Transport(transport, "auth"), Jar: jar, Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 25 {
			return errors.New("too many authentication redirects")
		}
		if !s.allowed(req.URL) {
			return errors.New("authentication redirected to an unexpected origin")
		}
		return nil
	}}
	return s, nil
}

func validatedURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid authentication server URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return nil, errors.New("authentication requires HTTPS")
	}
	return u, nil
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}
func (s *Session) allowed(u *url.URL) bool {
	return u.User == nil && (origin(u) == origin(s.base) || origin(u) == origin(s.iaaa))
}
func (s *Session) AccessToken() string { s.mu.Lock(); defer s.mu.Unlock(); return s.token }
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
	s.http.Jar, _ = cookiejar.New(nil)
	s.http.CloseIdleConnections()
}

func (s *Session) request(ctx context.Context, method, raw, ctype string, body io.Reader) ([]byte, *url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !s.allowed(u) {
		return nil, nil, errors.New("untrusted authentication endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, nil, errors.New("invalid authentication request")
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/155.0.0.0 Safari/537.36 pkudisk/1.0")
	resp, err := s.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			if endpoint, parseErr := url.Parse(ue.URL); parseErr == nil && s.allowed(endpoint) {
				u = endpoint
			}
			method = strings.ToUpper(ue.Op)
			err = ue.Err
		}
		return nil, nil, fmt.Errorf("authentication transport (%s %s%s): %w", method, u.Host, u.EscapedPath(), err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, &HTTPError{Status: resp.StatusCode, Method: method, Path: req.URL.Path}
	}
	return b, resp.Request.URL, nil
}
func (s *Session) get(ctx context.Context, u string) ([]byte, *url.URL, error) {
	return s.request(ctx, "GET", u, "", nil)
}
func (s *Session) form(ctx context.Context, u string, v url.Values) ([]byte, *url.URL, error) {
	return s.request(ctx, "POST", u, "application/x-www-form-urlencoded", strings.NewReader(v.Encode()))
}

type pageProps struct {
	Challenge string          `json:"challenge"`
	CSRF      string          `json:"csrftoken"`
	Device    json.RawMessage `json:"device"`
	Auth      struct {
		Third struct {
			Config struct {
				Server string `json:"authServer"`
			} `json:"config"`
		} `json:"thirdauth"`
		VCode struct {
			Enabled bool `json:"isenable"`
			Count   int  `json:"passwderrcnt"`
		} `json:"vcode_login_config"`
	} `json:"authconfig"`
}

func parsePage(data []byte) (pageProps, []string, error) {
	z := html.NewTokenizer(bytes.NewReader(data))
	var props pageProps
	var scripts []string
	found := false
	for {
		t := z.Next()
		if t == html.ErrorToken {
			if z.Err() != io.EOF {
				return props, nil, z.Err()
			}
			break
		}
		if t != html.StartTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) != "script" {
			continue
		}
		var id, src string
		for hasAttr {
			key, val, more := z.TagAttr()
			hasAttr = more
			switch string(key) {
			case "id":
				id = string(val)
			case "src":
				src = string(val)
			}
		}
		if src != "" {
			scripts = append(scripts, src)
		}
		if id == "__NEXT_DATA__" && z.Next() == html.TextToken {
			var v struct {
				Props struct {
					Page pageProps `json:"pageProps"`
				} `json:"props"`
			}
			if err := json.Unmarshal(z.Text(), &v); err != nil {
				return props, nil, err
			}
			props = v.Props.Page
			found = true
		}
	}
	if !found || props.Challenge == "" {
		return props, nil, errors.New("login page omitted the OAuth challenge")
	}
	return props, scripts, nil
}

func (s *Session) Login(ctx context.Context, credentials Credentials) (result error) {
	ctx, finish := s.trace.Begin(ctx, "authentication login")
	defer func() { finish("%s", diagnostic.Status(result)) }()
	if credentials.Username == "" || len(credentials.Password) == 0 {
		return errors.New("username and password are required")
	}
	s.trace.Printf(ctx, "waiting for authentication lock")
	s.mu.Lock()
	s.trace.Printf(ctx, "authentication lock acquired")
	defer s.mu.Unlock()
	b, _, err := s.get(ctx, s.base.String()+"/anyshare/oauth2/login?lang=zh-cn&redirect="+url.QueryEscape("/anyshare/"))
	if err != nil {
		return fmt.Errorf("open login: %w", err)
	}
	page, _, err := parsePage(b)
	if err != nil {
		return err
	}
	if err = s.loginIAAA(ctx, credentials, page); err != nil {
		return err
	}
	if err = s.updateToken(); err != nil {
		return err
	}
	return s.persistState()
}

func publicKey(raw string) (*rsa.PublicKey, error) {
	if !strings.Contains(raw, "-----BEGIN") {
		raw = "-----BEGIN PUBLIC KEY-----\n" + raw + "\n-----END PUBLIC KEY-----"
	}
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, errors.New("missing RSA public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		if k, e := x509.ParsePKCS1PublicKey(block.Bytes); e == nil {
			return k, nil
		}
		return nil, errors.New("invalid RSA public key")
	}
	k, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("login key is not RSA")
	}
	return k, nil
}

func encryptPassword(key *rsa.PublicKey, password []byte) (string, error) {
	b, err := rsa.EncryptPKCS1v15(rand.Reader, key, password)
	if err != nil {
		return "", errors.New("password cannot be encrypted with the server key")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func (s *Session) ask(ctx context.Context, kind string, image []byte) (string, error) {
	if s.challenge == nil {
		return "", fmt.Errorf("%s required; interactive verification is unavailable", kind)
	}
	return s.challenge(ctx, Challenge{Kind: kind, Image: image})
}

func parseForm(b []byte) (string, url.Values, error) {
	z := html.NewTokenizer(bytes.NewReader(b))
	values := url.Values{}
	action := ""
	for {
		t := z.Next()
		if t == html.ErrorToken {
			break
		}
		if t != html.StartTagToken && t != html.SelfClosingTagToken {
			continue
		}
		name, attrs := z.TagName()
		m := map[string]string{}
		for attrs {
			k, v, more := z.TagAttr()
			attrs = more
			m[string(k)] = string(v)
		}
		if string(name) == "form" {
			action = m["action"]
		}
		if string(name) == "input" && m["name"] != "" {
			values.Set(m["name"], m["value"])
		}
	}
	if action == "" || values.Get("appID") == "" {
		return "", nil, errors.New("SSO page omitted the IAAA form")
	}
	return action, values, nil
}

func (s *Session) loginIAAA(ctx context.Context, c Credentials, page pageProps) error {
	s.http.Jar.SetCookies(s.base, []*http.Cookie{{Name: "login_challenge", Value: page.Challenge, Path: "/"}, {Name: "is_previous_login_3rd_party", Value: "true", Path: "/"}})
	u, err := s.base.Parse(page.Auth.Third.Config.Server)
	if err != nil || page.Auth.Third.Config.Server == "" || origin(u) != origin(s.base) {
		return errors.New("login page omitted a trusted PKU SSO endpoint")
	}
	b, _, err := s.get(ctx, u.String())
	if err != nil {
		return err
	}
	action, values, err := parseForm(b)
	if err != nil {
		return err
	}
	actionURL, err := url.Parse(action)
	if err != nil || origin(actionURL) != origin(s.iaaa) {
		return errors.New("SSO form does not point to the expected IAAA origin")
	}
	redirect, err := url.Parse(values.Get("redirectUrl"))
	if err != nil || origin(redirect) != origin(s.base) {
		return errors.New("untrusted IAAA callback")
	}
	if _, _, err = s.form(ctx, action, values); err != nil {
		return err
	}
	b, _, err = s.get(ctx, s.iaaa.String()+"/iaaa/getPublicKey.do")
	if err != nil {
		return err
	}
	var keyInfo struct {
		Success bool   `json:"success"`
		Key     string `json:"key"`
	}
	if err = json.Unmarshal(b, &keyInfo); err != nil {
		return err
	}
	if !keyInfo.Success {
		return errors.New("IAAA did not provide a password encryption key")
	}
	key, err := publicKey(keyInfo.Key)
	if err != nil {
		return err
	}
	password, err := encryptPassword(key, c.Password)
	if err != nil {
		return err
	}
	b, _, err = s.get(ctx, s.iaaa.String()+"/iaaa/isShowCode.do")
	if err != nil {
		return err
	}
	var captcha struct {
		Success bool `json:"success"`
	}
	if err = json.Unmarshal(b, &captcha); err != nil {
		return err
	}
	if captcha.Success && c.Captcha == "" {
		image, _, err := s.get(ctx, s.iaaa.String()+"/iaaa/servlet/DrawServlet")
		if err != nil {
			return err
		}
		c.Captcha, err = s.ask(ctx, "CAPTCHA", image)
		if err != nil {
			return err
		}
	}
	q := url.Values{"userName": {c.Username}, "appId": {values.Get("appID")}}
	b, _, err = s.get(ctx, s.iaaa.String()+"/iaaa/isMobileAuthen.do?"+q.Encode())
	if err != nil {
		return err
	}
	var mobile struct {
		Success bool   `json:"success"`
		Needed  bool   `json:"isMobileAuthen"`
		Mode    string `json:"authenMode"`
	}
	if err = json.Unmarshal(b, &mobile); err != nil {
		return err
	}
	if mobile.Success && mobile.Needed {
		switch mobile.Mode {
		case "OTP":
			if c.OTP == "" {
				c.OTP, err = s.ask(ctx, "OTP", nil)
			}
		case "SMS":
			if c.SMS == "" {
				c.SMS, err = s.ask(ctx, "SMS (obtain the code using the official IAAA page)", nil)
			}
		default:
			return errors.New("unsupported IAAA second-factor method")
		}
		if err != nil {
			return err
		}
	}
	b, _, err = s.form(ctx, s.iaaa.String()+"/iaaa/oauthlogin.do", url.Values{"appid": {values.Get("appID")}, "userName": {c.Username}, "password": {password}, "randCode": {c.Captcha}, "smsCode": {c.SMS}, "otpCode": {c.OTP}, "remTrustChk": {"false"}, "redirUrl": {redirect.String()}})
	if err != nil {
		return err
	}
	var out struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
		Errors  struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err = json.Unmarshal(b, &out); err != nil {
		return err
	}
	if !out.Success || out.Token == "" {
		return fmt.Errorf("IAAA login rejected (code %s); check credentials and required verification", out.Errors.Code)
	}
	query := redirect.Query()
	query.Set("token", out.Token)
	query.Set("_rand", fmt.Sprint(time.Now().UnixNano()))
	redirect.RawQuery = query.Encode()
	_, _, err = s.get(ctx, redirect.String())
	return err
}

func (s *Session) updateToken() error {
	for _, path := range []string{"/", "/oauth2/", "/anyshare/"} {
		u, _ := s.base.Parse(path)
		for _, c := range s.http.Jar.Cookies(u) {
			if c.Name == "client.oauth2_token" && c.Value != "" {
				token, err := url.PathUnescape(c.Value)
				if err != nil {
					return errors.New("invalid access token encoding")
				}
				s.token = token
				return nil
			}
		}
	}
	return errors.New("login did not issue an AnyShare access token")
}

func (s *Session) Refresh(ctx context.Context, rejected string) (result error) {
	ctx, finish := s.trace.Begin(ctx, "renew authentication session")
	defer func() { finish("%s", diagnostic.Status(result)) }()
	var unlock io.Closer
	var err error
	if s.refreshLock != nil {
		s.trace.Printf(ctx, "waiting for saved-session lock")
		unlock, err = s.refreshLock(ctx)
		if err != nil {
			return fmt.Errorf("lock authentication session: %w", err)
		}
		defer unlock.Close()
		s.trace.Printf(ctx, "saved-session lock acquired")
	}
	return s.refresh(ctx, rejected, true)
}

// KeepAlive renews an idle session, including rotating refresh cookies in the
// same jar. Transient failures retry after a minute; a rejected session stops
// maintenance so the caller can unmount and let its supervisor log in again.
func (s *Session) KeepAlive(ctx context.Context, interval time.Duration, report func(error)) {
	if interval <= 0 {
		<-ctx.Done()
		return
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			err := s.Refresh(ctx, s.AccessToken())
			if ctx.Err() != nil {
				return
			}
			if report != nil {
				report(err)
			}
			if errors.Is(err, ErrSessionExpired) {
				return
			}
			delay := interval
			if err != nil {
				delay = min(interval, time.Minute)
			}
			timer.Reset(delay)
		}
	}
}
