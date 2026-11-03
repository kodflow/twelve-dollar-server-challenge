package api

import (
	"errors"
	"strings"

	"github.com/kitsunium/sdk/pkg/v1/crypto"
	"github.com/kitsunium/sdk/pkg/v1/errs"
	"github.com/kitsunium/sdk/pkg/v1/security/token"
)

// Identity is what an accepted token says about its holder. Username is a
// view of the scratch's claims buffer, valid until its next verification.
type Identity struct {
	UserID   int64
	Username []byte
}

// authResult names the SPEC's three 401 messages, plus success.
type authResult uint8

const (
	authOK authResult = iota
	authMissing
	authInvalid
	authPayload
)

// Auth verifies HS256 bearer tokens with the SDK verifier. Every call checks
// the signature again: nothing about a token is remembered between requests.
type Auth struct {
	verifier token.VerifierInto
}

// Scratch is one loop's verification state: the SDK's MAC scratch and the
// claims buffer it verifies into. It is not safe for concurrent use; each
// loop owns one.
type Scratch struct {
	mac    *token.VerifyScratch
	claims *token.ClaimsBuf
}

// usernameClaim is the one private claim the SPEC reads.
const usernameClaim = "username"

// NewScratch returns a scratch for one loop.
func (a *Auth) NewScratch() *Scratch {
	claims, err := token.NewClaimsBuf(usernameClaim)
	if err != nil {
		// A constant, valid claim name: only a broken SDK refuses it.
		panic(err)
	}
	return &Scratch{mac: a.verifier.NewScratch(), claims: claims}
}

// NewAuth builds the verifier for secret. The SDK takes exactly 32-byte keys;
// HMAC zero-pads a shorter key to the block size anyway (RFC 2104 §2), so
// padding the challenge's 23-byte secret to 32 bytes yields the same MAC.
// A secret longer than 32 bytes cannot be represented and is refused.
func NewAuth(secret []byte) (*Auth, error) {
	if len(secret) > crypto.KeyLen {
		return nil, errors.New("JWT_SECRET longer than 32 bytes is not supported")
	}
	raw := make([]byte, crypto.KeyLen)
	copy(raw, secret)
	key, err := crypto.NewKey(raw)
	if err != nil {
		return nil, err
	}
	// The zero VerifierConfig is the SPEC's profile: HS256 only, exp
	// required, no leeway.
	v, err := token.NewHS256Verifier(key, token.VerifierConfig{})
	if err != nil {
		return nil, err
	}
	into, ok := v.(token.VerifierInto)
	if !ok {
		return nil, errors.New("SDK HS256 verifier does not implement token.VerifierInto")
	}
	return &Auth{verifier: into}, nil
}

var bearer = []byte("Bearer ")

// check maps an Authorization header to an identity or to one of the SPEC's
// refusals, verifying with sc.
func (a *Auth) check(header []byte, present bool, sc *Scratch) (Identity, authResult) {
	if !present || len(header) < len(bearer) || string(header[:len(bearer)]) != string(bearer) {
		return Identity{}, authMissing
	}
	if err := a.verifier.VerifyInto(sc.mac, header[len(bearer):], sc.claims); err != nil {
		if subjectNotString(err) {
			return Identity{}, authPayload
		}
		return Identity{}, authInvalid
	}
	sub, _ := sc.claims.Text(token.TextSubject)
	uid, ok := positiveInt(sub)
	if !ok {
		return Identity{}, authPayload
	}
	// ClaimInvalid is the empty string: still a string, so accepted.
	name, state := sc.claims.Private(usernameClaim)
	if state != token.ClaimOK && state != token.ClaimInvalid {
		return Identity{}, authPayload
	}
	return Identity{UserID: uid, Username: name}, authOK
}

// subjectNotString reports the SDK's refusal of a signed token whose "sub" is
// not a JSON string. The SDK decodes claims only after the MAC verified, so
// this is a valid JWT with a bad payload, which the SPEC answers with
// "invalid token payload" rather than "invalid or expired token".
func subjectNotString(err error) bool {
	if !errors.Is(err, token.Malformed) {
		return false
	}
	for _, f := range errs.FieldsOf(err) {
		if f.Key() == "detail" && strings.HasPrefix(f.StringValue(), "registered claim sub ") {
			return true
		}
	}
	return false
}

// positiveInt parses a decimal integer > 0 written with digits only.
func positiveInt[T string | []byte](s T) (int64, bool) {
	if len(s) == 0 || len(s) > 18 {
		return 0, false
	}
	var n int64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, n > 0
}
