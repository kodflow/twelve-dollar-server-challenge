package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const secret = "twelve-dollar-challenge"

func sign(t *testing.T, header, payload string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	return input + "." + enc.EncodeToString(mac.Sum(nil))
}

func hs256(t *testing.T, payload string) string {
	t.Helper()
	return sign(t, `{"alg":"HS256","typ":"JWT"}`, payload)
}

func newAuth(t *testing.T) *Auth {
	t.Helper()
	a, err := NewAuth([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthCheck(t *testing.T) {
	a := newAuth(t)
	exp := time.Now().Add(time.Hour).Unix()
	past := time.Now().Add(-time.Hour).Unix()
	cases := []struct {
		name   string
		header string
		absent bool
		want   authResult
		user   string
		uid    int64
	}{
		{name: "absent", absent: true, want: authMissing},
		{name: "basic", header: "Basic abc", want: authMissing},
		{name: "lowercase scheme", header: "bearer " + hs256(t, fmt.Sprintf(`{"sub":"1","username":"a","exp":%d}`, exp)), want: authMissing},
		{name: "garbage", header: "Bearer abc", want: authInvalid},
		{name: "valid", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"7","username":"golden","exp":%d}`, exp)), want: authOK, user: "golden", uid: 7},
		{name: "empty username is a string", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"7","username":"","exp":%d}`, exp)), want: authOK, uid: 7},
		{name: "expired", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"1","username":"a","exp":%d}`, past)), want: authInvalid},
		{name: "no exp", header: "Bearer " + hs256(t, `{"sub":"1","username":"a"}`), want: authInvalid},
		{name: "alg none", header: "Bearer " + sign(t, `{"alg":"none","typ":"JWT"}`, fmt.Sprintf(`{"sub":"1","username":"a","exp":%d}`, exp)), want: authInvalid},
		{name: "sub not integer", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"abc","username":"a","exp":%d}`, exp)), want: authPayload},
		{name: "sub zero", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"0","username":"a","exp":%d}`, exp)), want: authPayload},
		{name: "sub negative", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"-1","username":"a","exp":%d}`, exp)), want: authPayload},
		{name: "sub number", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":1,"username":"a","exp":%d}`, exp)), want: authPayload},
		{name: "sub missing", header: "Bearer " + hs256(t, fmt.Sprintf(`{"username":"a","exp":%d}`, exp)), want: authPayload},
		{name: "no username", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"1","exp":%d}`, exp)), want: authPayload},
		{name: "username number", header: "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"1","username":123,"exp":%d}`, exp)), want: authPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			who, got := a.check([]byte(tc.header), !tc.absent, a.NewScratch())
			if got != tc.want {
				t.Fatalf("result = %d, want %d", got, tc.want)
			}
			if got == authOK && (string(who.Username) != tc.user || who.UserID != tc.uid) {
				t.Fatalf("identity = %+v", who)
			}
		})
	}
}

// TestSeedTokens checks the zero-padded key against tokens minted by
// seed/generate.mjs with the raw 23-byte secret.
func TestSeedTokens(t *testing.T) {
	raw, err := os.ReadFile("../../../../seed/tokens.json")
	if err != nil {
		t.Skip("seed/tokens.json not built: bash seed/make-seed.sh")
	}
	var tokens []struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		t.Fatal(err)
	}
	a := newAuth(t)
	sc := a.NewScratch()
	for _, tk := range tokens[:200] {
		who, res := a.check([]byte("Bearer "+tk.Token), true, sc)
		if res != authOK || who.UserID != tk.ID || string(who.Username) != tk.Username {
			t.Fatalf("token %d: result %d identity %+v", tk.ID, res, who)
		}
	}
}

func TestRoute(t *testing.T) {
	a := newAuth(t)
	sc := a.NewScratch()
	tok := "Bearer " + hs256(t, fmt.Sprintf(`{"sub":"3","username":"u","exp":%d}`, time.Now().Add(time.Hour).Unix()))
	cases := []struct {
		method, path, auth, body string
		kind                     Kind
		status                   int
		msg                      string
	}{
		{method: "GET", path: "/feed", kind: KindFeed},
		{method: "GET", path: "/health", kind: KindHealth},
		{method: "GET", path: "/posts/42", kind: KindGetPost},
		{method: "GET", path: "/posts/0", status: 400, msg: "invalid post id"},
		{method: "GET", path: "/posts/1.5", status: 400, msg: "invalid post id"},
		{method: "GET", path: "/posts/", status: 404, msg: "not found"},
		{method: "GET", path: "/nope", status: 404, msg: "not found"},
		{method: "POST", path: "/posts/abc/like", status: 401, msg: "missing bearer token"},
		{method: "POST", path: "/posts/abc/like", auth: tok, status: 400, msg: "invalid post id"},
		{method: "POST", path: "/posts/9/like", auth: tok, kind: KindLikePost},
		{method: "POST", path: "/posts", auth: tok, body: `{"body":"  hi  "}`, kind: KindCreatePost},
		{method: "POST", path: "/posts", auth: tok, body: `{bad`, status: 400, msg: "malformed JSON body"},
		{method: "POST", path: "/posts", auth: tok, body: `[1]`, status: 400, msg: "body is required"},
		{method: "POST", path: "/posts", auth: tok, body: "{\"body\":\"\\u3000\\ufeff\\u00a0\"}", status: 400, msg: "body is required"},
		{method: "POST", path: "/posts", auth: tok, body: `{"body":"` + strings.Repeat("é", 500) + `"}`, kind: KindCreatePost},
		{method: "POST", path: "/posts", auth: tok, body: `{"body":"` + strings.Repeat("é", 501) + `"}`, status: 400, msg: "body must be at most 500 characters"},
		{method: "POST", path: "/posts", body: `{bad`, status: 401, msg: "missing bearer token"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path+" "+tc.body, func(t *testing.T) {
			req := Request{Method: []byte(tc.method), Path: []byte(tc.path), Body: []byte(tc.body)}
			if tc.auth != "" {
				req.Auth, req.HasAuth = []byte(tc.auth), true
			}
			op := a.Route(&req, sc)
			if tc.status != 0 {
				want := fmt.Sprintf(`{"error":%q}`, tc.msg)
				if op.Kind != KindStatic || op.Status != tc.status || string(op.Body) != want {
					t.Fatalf("got %d %d %s", op.Kind, op.Status, op.Body)
				}
				return
			}
			if op.Kind != tc.kind {
				t.Fatalf("kind = %d, want %d (%d %s)", op.Kind, tc.kind, op.Status, op.Body)
			}
			if op.Kind == KindCreatePost && (string(op.User) != "u" || op.UserID != 3 || strings.TrimSpace(op.Text) != op.Text) {
				t.Fatalf("create op %+v", op)
			}
		})
	}
}
