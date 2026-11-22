package signins

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// GitHub's device code answer is taken only field by field within its
// bounds: a verification page on https://github.com alone, printable codes,
// a life of at most an hour, and an interval of at most 5 minutes, 5
// seconds when it names none (rule 69).
func TestParseCodeTakesOnlyWhatItChecks(t *testing.T) {
	c, err := parseCode([]byte(`{"device_code":"d","user_code":"AB-12","verification_uri":"https://github.com/login/device","expires_in":900}`))
	if err != nil || c.DeviceCode != "d" || c.UserCode != "AB-12" || c.VerificationURI != DevicePage || c.ExpiresIn != 900*time.Second || c.Interval != defaultInterval {
		t.Fatalf("parseCode %+v %v", c, err)
	}
	for _, bad := range []string{
		`[]`,
		`{"user_code":"AB","verification_uri":"https://github.com/login/device","expires_in":900}`,
		`{"device_code":"d","verification_uri":"https://github.com/login/device","expires_in":900}`,
		`{"device_code":"d","user_code":"AB 12","verification_uri":"https://github.com/login/device","expires_in":900}`,
		`{"device_code":"d","user_code":"` + strings.Repeat("A", maxCode+1) + `","verification_uri":"https://github.com/login/device","expires_in":900}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"http://github.com/login/device","expires_in":900}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://evil.example/login/device","expires_in":900}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://user@github.com/login/device","expires_in":900}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com.evil.example/","expires_in":900}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com/login/device"}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com/login/device","expires_in":0}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com/login/device","expires_in":3601}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com/login/device","expires_in":900.5}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com/login/device","expires_in":"900"}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com/login/device","expires_in":900,"interval":301}`,
		`{"device_code":"d","user_code":"AB","verification_uri":"https://github.com/login/device","expires_in":900,"interval":-1}`,
	} {
		if c, err := parseCode([]byte(bad)); err == nil {
			t.Errorf("parseCode took %s: %+v", bad, c)
		}
	}
}

// A poll's answer is a token, one of RFC 8628's errors, or GitHub's other
// error with the start of its description, printable only.
func TestParsePollReadsEachAnswer(t *testing.T) {
	cases := []struct {
		body string
		want Poll
	}{
		{granted, Poll{Token: "gho_granted_secret"}},
		{pending, Poll{Pending: true}},
		{slowDown, Poll{SlowDown: true, Interval: 10 * time.Second}},
		{`{"error":"slow_down","interval":"x"}`, Poll{SlowDown: true}},
		{denied, Poll{Denied: true}},
		{expired, Poll{Expired: true}},
		{`{"error":"device_flow_disabled","error_description":"Device flow\u0007 is off"}`, Poll{Refused: "GitHub ended the device flow: device_flow_disabled."}},
	}
	for _, c := range cases {
		if got, err := parsePoll([]byte(c.body), 200); err != nil || got != c.want {
			t.Errorf("parsePoll(%s) = %+v %v, want %+v", c.body, got, err, c.want)
		}
	}
	for _, bad := range []string{`nope`, `{}`, `{"access_token":"a b"}`, `{"access_token":""}`, `{"error":7}`} {
		if got, err := parsePoll([]byte(bad), 200); err == nil {
			t.Errorf("parsePoll took %s: %+v", bad, got)
		}
	}
}

// The account is a GitHub login, or none.
func TestUserReadsALogin(t *testing.T) {
	g := newFakeGitHub(t)
	for body, want := range map[string]string{userAnswer: "octocat", `{"login":"-bad"}`: "", `{"login":"a--b"}`: "", `{"id":1}`: "", `{"login":"a-b-c"}`: "a-b-c"} {
		g.user = ok(body)
		if got, _ := g.client().User(context.Background(), "t"); got != want {
			t.Errorf("User(%s) = %q, want %q", body, got, want)
		}
	}
}

// A device flow's requests go through no proxy and follow no redirect,
// and an answer over its bound is refused.
func TestTheDeviceFlowsClientStaysOnGitHub(t *testing.T) {
	c := DefaultHTTP()
	if c.Transport.(*http.Transport).Proxy != nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse || c.Timeout != requestTimeout {
		t.Fatalf("the client %+v", c)
	}
	g := newFakeGitHub(t)
	g.code = ok(`{"device_code":"` + strings.Repeat("x", maxBody) + `"}`)
	if _, err := g.client().Start(context.Background()); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("a long answer: %v", err)
	}
}

// The state of a stored sign-in, and the account shown beside it: only
// one kept with the same credential (rule 69).
func TestStateAndAccount(t *testing.T) {
	cases := []struct {
		s    Stored
		want proto.SignInState
	}{
		{Stored{}, proto.SignInNone},
		{Stored{Kind: KindAPIKey}, proto.SignInNone},
		{Stored{Kind: KindOAuth, Refreshable: true}, proto.SignInSignedIn},
		{Stored{Kind: KindOAuth, Refreshable: true, Expired: true}, proto.SignInSignedIn},
		{Stored{Kind: KindOAuth, Expired: true}, proto.SignInExpired},
		{Stored{Kind: KindOAuth}, proto.SignInSignedIn},
	}
	for _, c := range cases {
		if got := State(c.s); got != c.want {
			t.Errorf("State(%+v) = %s, want %s", c.s, got, c.want)
		}
	}
	signedIn := Stored{Kind: KindOAuth, Refreshable: true, Identity: "id1"}
	if Account(signedIn, "id1", "octocat") != "octocat" || Account(signedIn, "id0", "octocat") != "" ||
		Account(Stored{}, "", "octocat") != "" || Account(Stored{Kind: KindOAuth}, "", "octocat") != "" {
		t.Fatal("Account")
	}
	if p, ok := Find(Copilot); !ok || !p.Device || Providers[0].ID != Copilot {
		t.Fatal("Copilot is not the first provider, signed in with a device flow")
	}
	for _, p := range Providers[1:] {
		if p.Device {
			t.Errorf("%s signs in with a device flow", p.ID)
		}
	}
}
