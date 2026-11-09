// Package sigv4 signs HTTP requests with AWS Signature Version 4, header
// form, as specified in
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html
//
// It covers what JSON APIs such as Bedrock Runtime need: every header
// present at signing time is signed (except the few that proxies and
// transports rewrite), the payload hash goes into the canonical request but
// not into a header, and the path is escaped once more on top of its wire
// form, as AWS does for every service except S3.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Algorithm is the SigV4 algorithm identifier.
const Algorithm = "AWS4-HMAC-SHA256"

// Header names set by Sign.
const (
	HeaderDate          = "X-Amz-Date"
	HeaderSecurityToken = "X-Amz-Security-Token"
	HeaderAuthorization = "Authorization"
)

// Credentials are AWS access keys; SessionToken is set for temporary
// (STS) credentials.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// ignored headers are left out of the signature: hop-by-hop or rewritten
// by transports and proxies (the AWS SDKs skip the same set).
var ignored = map[string]bool{
	"authorization":     true,
	"user-agent":        true,
	"x-amzn-trace-id":   true,
	"expect":            true,
	"transfer-encoding": true,
	"connection":        true,
	"content-length":    true,
}

// Sign sets X-Amz-Date, X-Amz-Security-Token (temporary credentials only)
// and Authorization on r. body must be exactly the payload r carries.
func Sign(r *http.Request, body []byte, c Credentials, region, service string, now time.Time) error {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return errors.New("sigv4: access key id and secret access key required")
	}
	if region == "" || service == "" {
		return errors.New("sigv4: region and service required")
	}
	if r.Header == nil {
		r.Header = http.Header{}
	}
	t := now.UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	r.Header.Del(HeaderAuthorization)
	r.Header.Set(HeaderDate, amzDate)
	r.Header.Del(HeaderSecurityToken)
	if c.SessionToken != "" {
		r.Header.Set(HeaderSecurityToken, c.SessionToken)
	}

	headers, signed := canonicalHeaders(r)
	sum := sha256.Sum256(body)
	creq := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL),
		canonicalQuery(r.URL.RawQuery),
		headers,
		signed,
		hex.EncodeToString(sum[:]),
	}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	creqSum := sha256.Sum256([]byte(creq))
	toSign := Algorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(creqSum[:])

	key := mac([]byte("AWS4"+c.SecretAccessKey), day)
	key = mac(key, region)
	key = mac(key, service)
	key = mac(key, "aws4_request")
	sig := hex.EncodeToString(mac(key, toSign))
	r.Header.Set(HeaderAuthorization, Algorithm+" Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
	return nil
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalHeaders returns the canonical header block (each line ending in
// "\n") and the signed header list.
func canonicalHeaders(r *http.Request) (string, string) {
	vals := map[string][]string{}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if ignored[lk] || lk == "host" {
			continue
		}
		vals[lk] = append(vals[lk], vs...)
	}
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	vals["host"] = []string{host}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		trimmed := make([]string, len(vals[k]))
		for i, v := range vals[k] {
			trimmed[i] = strings.Join(strings.Fields(v), " ")
		}
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strings.Join(trimmed, ","))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

// canonicalURI escapes the wire form of the path once more, segment by
// segment. A model id "a:1" travels as "/model/a%3A1/invoke" and is signed
// as "/model/a%253A1/invoke".
func canonicalURI(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts the decoded parameters by escaped name, then value.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		q = url.Values{}
	}
	type pair struct{ k, v string }
	var pairs []pair
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, pair{escape(k), escape(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

// escape percent-encodes every byte outside the unreserved set
// A-Z a-z 0-9 - _ . ~, with upper-case hex.
func escape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}
