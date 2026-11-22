package signins

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// GitHub's origins, and the device page a code is entered at. pocketd
// sends a device flow's requests nowhere else, and takes a verification
// page on no other origin.
const (
	gitHubWeb = "https://github.com"
	gitHubAPI = "https://api.github.com"
	// DevicePage is GitHub's device page, where the owner enters a code.
	DevicePage = gitHubWeb + "/login/device"
)

// copilotClientID is the OAuth app Copilot's device flow signs in to:
// PiG's, upstream pi-ai's, which is VS Code's Copilot Chat. Copilot takes
// the GitHub token only of this app in exchange for its own (PiG's
// LoginGitHubCopilot; ADR-0005 names the client identity PiG sends). Kept
// encoded as PiG keeps it.
var copilotClientID = func() string {
	id, err := base64.StdEncoding.DecodeString("SXYxLmI1MDdhMDhjODdlY2ZlOTg=")
	if err != nil {
		panic(err)
	}
	return string(id)
}()

// userAgent is the client PiG names on GitHub's device flow requests.
const userAgent = "GitHubCopilotChat/0.35.0"

// The bounds of what pocketd takes from GitHub's device flow (rule 69).
const (
	// defaultInterval is the wait between polls when GitHub names none
	// (RFC 8628, 3.2), and slowDownStep what a slow_down adds to it (3.5).
	defaultInterval = 5 * time.Second
	slowDownStep    = 5 * time.Second
	// maxExpiresIn and maxInterval bound the code's life and the wait
	// GitHub asks for; maxWait bounds the wait any slow_down makes.
	maxExpiresIn = time.Hour
	maxInterval  = 5 * time.Minute
	// maxCode bounds a user code, maxSecret a device code and a token, and
	// maxBody an answer's body.
	maxCode   = 32
	maxSecret = 4096
	maxBody   = 64 << 10
	// maxDescription bounds the part of GitHub's error_description a
	// failed flow says.
	maxDescription = 200
	// requestTimeout bounds each request to GitHub.
	requestTimeout = 30 * time.Second
)

// GitHub runs GitHub's device flow for Copilot's OAuth app, as PiG's own
// sign-in does, with every answer checked field by field.
type GitHub struct {
	// HTTP carries the requests; nil is DefaultHTTP. Tests give a fake
	// GitHub of their own.
	HTTP *http.Client
}

// DefaultHTTP is the client a device flow's requests go through: no
// proxy, whatever HTTP_PROXY says, no redirect, and a bound on each
// request. A token goes to GitHub's own origin alone.
func DefaultHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return &http.Client{Transport: t, Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// sharedHTTP is the one DefaultHTTP a GitHub with no client of its own
// uses, so its connections are kept.
var sharedHTTP = sync.OnceValue(DefaultHTTP)

func (g *GitHub) client() *http.Client {
	if g == nil || g.HTTP == nil {
		return sharedHTTP()
	}
	return g.HTTP
}

// Code is a device flow's start, as GitHub answered it.
type Code struct {
	// DeviceCode is what pocketd polls with; it never leaves pocketd.
	DeviceCode string
	// UserCode is what the owner enters at VerificationURI.
	UserCode, VerificationURI string
	// ExpiresIn is how long the code holds, and Interval the wait between
	// polls.
	ExpiresIn, Interval time.Duration
}

// Poll is one poll's answer: exactly one of its outcomes.
type Poll struct {
	// Token is the GitHub token of an approved code.
	Token string
	// Pending says the code still waits; SlowDown that pocketd polls too
	// often, with the interval GitHub names (0 for none).
	Pending  bool
	SlowDown bool
	Interval time.Duration
	// Denied says the owner denied the code, Expired that it expired.
	Denied, Expired bool
	// Refused is GitHub's error for any other end, as the flow says it.
	Refused string
}

// errTransient is a poll that did not reach an answer: GitHub could not be
// reached, or failed (5xx, 429). The flow polls again (maxFailures).
var errTransient = errors.New("GitHub could not be reached")

// Start asks GitHub for a device code, for Copilot's app with the
// read:user scope, as PiG's sign-in does.
func (g *GitHub) Start(ctx context.Context) (Code, error) {
	body, status, err := g.post(ctx, gitHubWeb+"/login/device/code", url.Values{"client_id": {copilotClientID}, "scope": {"read:user"}})
	if err != nil {
		return Code{}, err
	}
	if status/100 != 2 {
		return Code{}, fmt.Errorf("GitHub answered %d to the device code request", status)
	}
	return parseCode(body)
}

// parseCode checks GitHub's device code answer, field by field: a device
// code and a user code of printable characters within their bounds, a
// verification page on GitHub's own origin over HTTPS, a life of at most
// an hour, and a wait between polls of at most 5 minutes (5 seconds when
// GitHub names none).
func parseCode(body []byte) (Code, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return Code{}, errors.New("GitHub's device code answer is no JSON object")
	}
	var c Code
	var err error
	if c.DeviceCode, err = stringField(raw, "device_code", maxSecret); err != nil {
		return Code{}, err
	}
	if c.UserCode, err = stringField(raw, "user_code", maxCode); err != nil {
		return Code{}, err
	}
	uri, err := stringField(raw, "verification_uri", 2048)
	if err != nil {
		return Code{}, err
	}
	if c.VerificationURI, err = verificationPage(uri); err != nil {
		return Code{}, err
	}
	expires, ok, err := secondsField(raw, "expires_in")
	switch {
	case err != nil:
		return Code{}, err
	case !ok || expires <= 0 || expires > maxExpiresIn:
		return Code{}, fmt.Errorf("GitHub's device code answer gives no expires_in from 1 second to %s", maxExpiresIn)
	}
	c.ExpiresIn = expires
	interval, ok, err := secondsField(raw, "interval")
	switch {
	case err != nil:
		return Code{}, err
	case !ok || interval == 0:
		interval = defaultInterval
	case interval < 0 || interval > maxInterval:
		return Code{}, fmt.Errorf("GitHub's device code answer asks for an interval outside 1 second to %s", maxInterval)
	}
	c.Interval = interval
	return c, nil
}

// verificationPage is uri when it is an HTTPS page on GitHub's own
// origin, with no user information, as GitHub's device page is.
func verificationPage(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Opaque != "" {
		return "", errors.New("GitHub's device code answer names a verification page that is not on https://github.com")
	}
	return u.String(), nil
}

// Poll asks GitHub whether the owner approved the device code (RFC 8628,
// 3.4 and 3.5, as GitHub answers it). A request that reached no answer,
// a 5xx or a 429 is errTransient.
func (g *GitHub) Poll(ctx context.Context, deviceCode string) (Poll, error) {
	body, status, err := g.post(ctx, gitHubWeb+"/login/oauth/access_token", url.Values{
		"client_id": {copilotClientID}, "device_code": {deviceCode},
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"},
	})
	if err != nil {
		if ctx.Err() != nil {
			return Poll{}, ctx.Err()
		}
		return Poll{}, fmt.Errorf("%w: %v", errTransient, err)
	}
	if status >= 500 || status == http.StatusTooManyRequests {
		return Poll{}, fmt.Errorf("%w: it answered %d", errTransient, status)
	}
	return parsePoll(body, status)
}

// parsePoll checks GitHub's answer to a poll: a token of printable
// characters within its bound, or one of RFC 8628's errors. Any other
// error ends the flow with GitHub's code and the start of its
// description.
func parsePoll(body []byte, status int) (Poll, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return Poll{}, fmt.Errorf("GitHub's answer to a poll (%d) is no JSON object", status)
	}
	if _, ok := raw["access_token"]; ok {
		token, err := stringField(raw, "access_token", maxSecret)
		if err != nil {
			return Poll{}, err
		}
		return Poll{Token: token}, nil
	}
	code, err := stringField(raw, "error", 64)
	if err != nil {
		return Poll{}, fmt.Errorf("GitHub's answer to a poll (%d) has neither a token nor an error", status)
	}
	switch code {
	case "authorization_pending":
		return Poll{Pending: true}, nil
	case "slow_down":
		interval, _, err := secondsField(raw, "interval")
		if err != nil || interval < 0 || interval > maxInterval {
			interval = 0
		}
		return Poll{SlowDown: true, Interval: interval}, nil
	case "access_denied":
		return Poll{Denied: true}, nil
	case "expired_token":
		return Poll{Expired: true}, nil
	}
	refused := "GitHub ended the device flow: " + code
	var description string
	if json.Unmarshal(raw["error_description"], &description) == nil {
		if d := printable(description, maxDescription); d != "" {
			refused += ": " + d
		}
	}
	return Poll{Refused: refused}, nil
}

// SlowedDown is the wait between polls after a slow_down: the wait so far
// with slowDownStep more (RFC 8628, 3.5), or the interval GitHub named
// when it is longer, at most maxInterval.
func SlowedDown(current, named time.Duration) time.Duration {
	next := current + slowDownStep
	if named > next {
		next = named
	}
	return min(next, maxInterval)
}

// User is the login of the GitHub account token signs in, from GitHub's
// API; "" with the reason when it gives none a login can be.
func (g *GitHub) User(ctx context.Context, token string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, gitHubAPI+"/user", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", userAgent)
	body, status, err := g.do(request)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %d to the account's read", status)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &user); err != nil || !loginPattern.MatchString(user.Login) {
		return "", errors.New("GitHub's account answer names no login")
	}
	return user.Login, nil
}

// loginPattern is a GitHub login: 1 to 39 letters, digits and single
// hyphens, neither first nor last.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// post sends form to target as GitHub's device flow takes it, and reads
// at most maxBody of the answer.
func (g *GitHub) post(ctx context.Context, target string, form url.Values) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", userAgent)
	return g.do(request)
}

func (g *GitHub) do(request *http.Request) ([]byte, int, error) {
	response, err := g.client().Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > maxBody {
		return nil, 0, fmt.Errorf("GitHub's answer is over %d bytes", maxBody)
	}
	return body, response.StatusCode, nil
}

// stringField is raw's field name: a string of 1 to most printable ASCII
// characters, with no space.
func stringField(raw map[string]json.RawMessage, name string, most int) (string, error) {
	var value string
	if err := json.Unmarshal(raw[name], &value); err != nil || value == "" {
		return "", fmt.Errorf("GitHub's answer has no %s", name)
	}
	if len(value) > most {
		return "", fmt.Errorf("GitHub's answer has a %s over %d characters", name, most)
	}
	for _, r := range value {
		if r <= ' ' || r > '~' {
			return "", fmt.Errorf("GitHub's answer has a %s with a character pocketd does not take", name)
		}
	}
	return value, nil
}

// secondsField is raw's field name as a whole number of seconds: false
// when it is absent or null, an error when it is no number or not whole.
func secondsField(raw map[string]json.RawMessage, name string) (time.Duration, bool, error) {
	value, ok := raw[name]
	if !ok || string(value) == "null" {
		return 0, false, nil
	}
	var n float64
	if err := json.Unmarshal(value, &n); err != nil || n != math.Trunc(n) || math.Abs(n) > 1e6 {
		return 0, false, fmt.Errorf("GitHub's answer has a %s that is no whole number of seconds", name)
	}
	return time.Duration(n) * time.Second, true, nil
}

// printable is text with every character outside printable ASCII
// dropped, cut to most bytes.
func printable(text string, most int) string {
	var b strings.Builder
	for _, r := range text {
		if r >= ' ' && r <= '~' {
			b.WriteRune(r)
		}
		if b.Len() >= most {
			break
		}
	}
	return strings.TrimSpace(b.String())
}
