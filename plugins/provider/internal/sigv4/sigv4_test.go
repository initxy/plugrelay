package sigv4

import (
	"bytes"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Vectors from the AWS SigV4 test suite, as redistributed by botocore
// (Apache-2.0): https://github.com/boto/botocore/tree/develop/tests/unit/auth/aws4_testsuite
// All use the suite's example credentials, region us-east-1, service
// "service" and the date 20150830T123600Z. Vectors whose request line is
// not a valid URL (raw UTF-8 or unnormalized paths, multi-line headers)
// are left out: Go cannot send them.
var suiteCreds = Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

var suiteTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

func TestAWSTestSuite(t *testing.T) {
	const prefix = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, "
	type hdr struct{ k, v string }
	cases := []struct {
		name    string
		method  string
		target  string
		headers []hdr
		body    string
		token   string
		want    string
	}{
		{name: "get-vanilla", method: "GET", target: "/",
			want: "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{name: "get-vanilla-query", method: "GET", target: "/",
			want: "SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{name: "get-vanilla-empty-query-key", method: "GET", target: "/?Param1=value1",
			want: "SignedHeaders=host;x-amz-date, Signature=a67d582fa61cc504c4bae71f336f98b97f1ea3c7a6bfe1b6e45aec72011b9aeb"},
		{name: "get-vanilla-query-order-key", method: "GET", target: "/?Param1=value2&Param1=Value1",
			want: "SignedHeaders=host;x-amz-date, Signature=eedbc4e291e521cf13422ffca22be7d2eb8146eecf653089df300a15b2382bd1"},
		{name: "get-vanilla-query-order-value", method: "GET", target: "/?Param1=value2&Param1=value1",
			want: "SignedHeaders=host;x-amz-date, Signature=5772eed61e12b33fae39ee5e7012498b51d56abc0abb7c60486157bd471c4694"},
		{name: "get-vanilla-query-order-encoded", method: "GET", target: "/?Param-3=Value3&Param=Value2&%E1%88%B4=Value1",
			want: "SignedHeaders=host;x-amz-date, Signature=371d3713e185cc334048618a97f809c9ffe339c62934c032af5a0e595648fcac"},
		{name: "get-vanilla-query-unreserved", method: "GET",
			target: "/?-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz=-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
			want:   "SignedHeaders=host;x-amz-date, Signature=9c3e54bfcdf0b19771a7f523ee5669cdf59bc7cc0884027167c21bb143a40197"},
		{name: "get-vanilla-utf8-query", method: "GET", target: "/?%E1%88%B4=bar",
			want: "SignedHeaders=host;x-amz-date, Signature=2cdec8eed098649ff3a119c94853b13c643bcf08f8b0a1d91e12c9027818dd04"},
		{name: "get-unreserved", method: "GET", target: "/-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
			want: "SignedHeaders=host;x-amz-date, Signature=07ef7494c76fa4850883e2b006601f940f8a34d404d0cfa977f52a65bbf5f24f"},
		{name: "get-header-value-order", method: "GET", target: "/",
			headers: []hdr{{"My-Header1", "value4"}, {"My-Header1", "value1"}, {"My-Header1", "value3"}, {"My-Header1", "value2"}},
			want:    "SignedHeaders=host;my-header1;x-amz-date, Signature=08c7e5a9acfcfeb3ab6b2185e75ce8b1deb5e634ec47601a50643f830c755c01"},
		{name: "get-header-value-trim", method: "GET", target: "/",
			headers: []hdr{{"My-Header1", " value1"}, {"My-Header2", ` "a   b   c"`}},
			want:    "SignedHeaders=host;my-header1;my-header2;x-amz-date, Signature=acc3ed3afb60bb290fc8d2dd0098b9911fcaa05412b367055dee359757a9c736"},
		{name: "get-vanilla-with-session-token", method: "GET", target: "/",
			token: "6e86291e8372ff2a2260956d9b8aae1d763fbf315fa00fa31553b73ebf194267",
			want:  "SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=07ec1639c89043aa0e3e2de82b96708f198cceab042d4a97044c66dd9f74e7f8"},
		{name: "post-vanilla", method: "POST", target: "/",
			want: "SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"},
		{name: "post-vanilla-query", method: "POST", target: "/?Param1=value1",
			want: "SignedHeaders=host;x-amz-date, Signature=28038455d6de14eafc1f9222cf5aa6f1a96197d7deb8263271d420d138af7f11"},
		{name: "post-header-key-sort", method: "POST", target: "/",
			headers: []hdr{{"My-Header1", "value1"}},
			want:    "SignedHeaders=host;my-header1;x-amz-date, Signature=c5410059b04c1ee005303aed430f6e6645f61f4dc9e1461ec8f8916fdf18852c"},
		{name: "post-header-value-case", method: "POST", target: "/",
			headers: []hdr{{"My-Header1", "VALUE1"}},
			want:    "SignedHeaders=host;my-header1;x-amz-date, Signature=cdbc9802e29d2942e5e10b5bccfdd67c5f22c7c4e8ae67b53629efa58b974b7d"},
		{name: "post-x-www-form-urlencoded", method: "POST", target: "/",
			headers: []hdr{{"Content-Type", "application/x-www-form-urlencoded"}}, body: "Param1=value1",
			want: "SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"},
		{name: "post-x-www-form-urlencoded-parameters", method: "POST", target: "/",
			headers: []hdr{{"Content-Type", "application/x-www-form-urlencoded; charset=utf8"}}, body: "Param1=value1",
			want: "SignedHeaders=content-type;host;x-amz-date, Signature=1a72ec8f64bd914b0e42e42607c7fbce7fb2c7465f63e3092b3b0d39fa77a6fe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := http.NewRequest(c.method, "https://example.amazonaws.com"+c.target, bytes.NewReader([]byte(c.body)))
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range c.headers {
				r.Header.Add(h.k, h.v)
			}
			r.Header.Set("User-Agent", "not-signed/1")
			creds := suiteCreds
			creds.SessionToken = c.token
			if err := Sign(r, []byte(c.body), creds, "us-east-1", "service", suiteTime); err != nil {
				t.Fatal(err)
			}
			if got := r.Header.Get("Authorization"); got != prefix+c.want {
				t.Errorf("Authorization\n got %s\nwant %s", got, prefix+c.want)
			}
			if got := r.Header.Get(HeaderDate); got != "20150830T123600Z" {
				t.Errorf("X-Amz-Date = %q", got)
			}
		})
	}
}

// Every service but S3 signs the path escaped once more on top of its wire
// form, so a Bedrock model id with a colon is signed as %253A.
func TestCanonicalURIDoubleEscapes(t *testing.T) {
	u := &url.URL{Path: "/model/anthropic.claude-v2:1/invoke", RawPath: "/model/anthropic.claude-v2%3A1/invoke"}
	if got, want := canonicalURI(u), "/model/anthropic.claude-v2%253A1/invoke"; got != want {
		t.Errorf("canonicalURI = %s, want %s", got, want)
	}
	if got := canonicalURI(&url.URL{}); got != "/" {
		t.Errorf("empty path = %q", got)
	}
}

func TestSignResignsCleanly(t *testing.T) {
	r, _ := http.NewRequest("POST", "https://example.amazonaws.com/", nil)
	r.Header.Set("Authorization", "Bearer stale")
	r.Header.Set(HeaderSecurityToken, "stale")
	if err := Sign(r, nil, suiteCreds, "us-east-1", "service", suiteTime); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get(HeaderSecurityToken) != "" {
		t.Error("stale security token kept")
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b"
	if got := r.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %s", got)
	}
	if err := Sign(r, nil, Credentials{}, "us-east-1", "service", suiteTime); err == nil {
		t.Error("empty credentials accepted")
	}
}
