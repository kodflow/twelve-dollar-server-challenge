package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Kind is the database operation a request needs, or KindStatic for a
// response decided without touching the database.
type Kind uint8

const (
	KindStatic Kind = iota
	KindHealth
	KindFeed
	KindGetPost
	KindCreatePost
	KindLikePost
)

// Op is a routed request. For KindStatic, Status and Body are the response;
// otherwise the executor runs it and supplies them.
type Op struct {
	Kind   Kind
	Status int
	Body   []byte
	PostID int64
	UserID int64
	// Text is the trimmed post body and User the token's username, for
	// KindCreatePost. User is a view of the loop's claims buffer, valid until
	// that loop verifies its next token.
	Text string
	User []byte
}

// Request is what routing reads of a request the codec framed. Its slices
// alias the connection's bytes.
type Request struct {
	Method []byte
	// Path is the request-target without its query.
	Path []byte
	// Auth is the Authorization value, valid when HasAuth.
	Auth    []byte
	HasAuth bool
	Body    []byte
}

// MaxPostChars is the SPEC's limit on a trimmed post body.
const MaxPostChars = 500

// The SPEC's fixed error bodies.
var (
	errNotFound      = []byte(`{"error":"not found"}`)
	errInvalidID     = []byte(`{"error":"invalid post id"}`)
	errMissingBearer = []byte(`{"error":"missing bearer token"}`)
	errInvalidToken  = []byte(`{"error":"invalid or expired token"}`)
	errBadPayload    = []byte(`{"error":"invalid token payload"}`)
	errMalformedJSON = []byte(`{"error":"malformed JSON body"}`)
	errBodyRequired  = []byte(`{"error":"body is required"}`)
	errBodyTooLong   = []byte(`{"error":"body must be at most 500 characters"}`)
)

func static(status int, body []byte) Op {
	return Op{Kind: KindStatic, Status: status, Body: body}
}

// Route maps a parsed request to an operation, verifying tokens with sc. On
// authenticated routes it checks the token first and the id second, as the
// SPEC orders them.
func (a *Auth) Route(req *Request, sc *Scratch) Op {
	path := req.Path
	switch string(req.Method) {
	case "GET":
		switch {
		case string(path) == "/feed":
			return Op{Kind: KindFeed}
		case string(path) == "/health":
			return Op{Kind: KindHealth}
		case bytes.HasPrefix(path, []byte("/posts/")):
			id := path[len("/posts/"):]
			if len(id) == 0 || bytes.IndexByte(id, '/') >= 0 {
				return static(404, errNotFound)
			}
			pid, ok := positiveInt(id)
			if !ok {
				return static(400, errInvalidID)
			}
			return Op{Kind: KindGetPost, PostID: pid}
		}
	case "POST":
		if string(path) == "/posts" {
			who, res := a.check(req.Auth, req.HasAuth, sc)
			if res != authOK {
				return authFailure(res)
			}
			text, failure, ok := validateCreate(req.Body)
			if !ok {
				return failure
			}
			return Op{Kind: KindCreatePost, UserID: who.UserID, Text: text, User: who.Username}
		}
		if rest, ok := bytes.CutPrefix(path, []byte("/posts/")); ok {
			id, ok := bytes.CutSuffix(rest, []byte("/like"))
			if !ok || len(id) == 0 || bytes.IndexByte(id, '/') >= 0 {
				return static(404, errNotFound)
			}
			who, res := a.check(req.Auth, req.HasAuth, sc)
			if res != authOK {
				return authFailure(res)
			}
			pid, ok := positiveInt(id)
			if !ok {
				return static(400, errInvalidID)
			}
			return Op{Kind: KindLikePost, PostID: pid, UserID: who.UserID}
		}
	}
	return static(404, errNotFound)
}

func authFailure(res authResult) Op {
	switch res {
	case authMissing:
		return static(401, errMissingBearer)
	case authPayload:
		return static(401, errBadPayload)
	default:
		return static(401, errInvalidToken)
	}
}

// validateCreate checks a POST /posts body and returns the trimmed text.
func validateCreate(body []byte) (text string, failure Op, ok bool) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", static(400, errMalformedJSON), false
	}
	obj, isObj := doc.(map[string]any)
	if !isObj {
		return "", static(400, errBodyRequired), false
	}
	s, isStr := obj["body"].(string)
	if !isStr {
		return "", static(400, errBodyRequired), false
	}
	s = strings.TrimFunc(s, isTrimSpace)
	if s == "" {
		return "", static(400, errBodyRequired), false
	}
	// Characters, as the schema's CHECK (length(body) <= 500) counts them.
	if utf8.RuneCountInString(s) > MaxPostChars {
		return "", static(400, errBodyTooLong), false
	}
	return s, Op{}, true
}

// isTrimSpace is the set String.prototype.trim removes (ECMAScript WhiteSpace
// and LineTerminator), which is what "trimmed" means for the reference
// implementations.
func isTrimSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}
