package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	algorithm     = "AWS4-HMAC-SHA256"
	service       = "s3"
	emptySHA256   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedValue = "UNSIGNED-PAYLOAD"
)

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// signingKey derives the AWS4 signing key for a date/region/service.
func signingKey(secret, date, region, svc string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, svc)
	return hmacSHA256(kService, "aws4_request")
}

// uriEncode implements the AWS canonical URI/query escaping. Unreserved
// characters are A-Z a-z 0-9 - _ . ~; everything else is percent-encoded
// uppercase. Forward slashes are preserved unless encodeSlash is set.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalQuery sorts and encodes query parameters per SigV4.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// sign adds the SigV4 headers to req. payloadHash is the lowercase hex
// SHA-256 of the request body (emptySHA256 for none, or unsignedValue).
// signedHeaderNames returns the additional headers included in the signature
// (host is always included); content-type is added when present and signed.
func sign(req *http.Request, accessKey, secret, region string, payloadHash string, now time.Time, extraSigned map[string]string) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	// Build the set of headers to sign: host + the amz headers + extras.
	signed := map[string]string{"host": req.Host}
	if signed["host"] == "" {
		signed["host"] = req.URL.Host
	}
	signed["x-amz-content-sha256"] = payloadHash
	signed["x-amz-date"] = amzDate
	for k, v := range extraSigned {
		signed[strings.ToLower(k)] = v
	}

	names := make([]string, 0, len(signed))
	for k := range signed {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k)
		ch.WriteByte(':')
		ch.WriteString(strings.TrimSpace(signed[k]))
		ch.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalURI := uriEncode(req.URL.Path, false)
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery(req.URL.Query()),
		ch.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	sig := hex.EncodeToString(hmacSHA256(signingKey(secret, dateStamp, region, service), stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, accessKey, scope, signedHeaders, sig))
}
