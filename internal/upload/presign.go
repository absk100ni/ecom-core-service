package upload

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// presignS3URL builds an AWS Signature V4 presigned URL (query-string auth)
// using only the standard library. Equivalent to the SDK's PresignPutObject
// for simple PUT/GET with UNSIGNED-PAYLOAD and only the `host` header signed
// (the uploader may send any Content-Type since it is not part of the signature).
//
// Reference: https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html
func presignS3URL(method, host, keyPath, region, accessKey, secretKey, sessionToken string, expires time.Duration, now time.Time) string {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", fmt.Sprintf("%d", int(expires.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	if sessionToken != "" {
		q.Set("X-Amz-Security-Token", sessionToken)
	}
	canonicalQuery := canonicalQueryString(q)

	canonicalPath := canonicalURIPath(keyPath)
	canonicalRequest := strings.Join([]string{
		method,
		canonicalPath,
		canonicalQuery,
		"host:" + host,
		"", // end of headers
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	hashedCR := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(hashedCR[:]),
	}, "\n")

	// Signing key derivation chain
	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return fmt.Sprintf("https://%s%s?%s&X-Amz-Signature=%s", host, canonicalPath, canonicalQuery, signature)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQueryString encodes params per SigV4 rules: RFC 3986 strict
// encoding (spaces as %20, '/' encoded in values), sorted by key.
func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(q.Get(k), true))
	}
	return strings.Join(parts, "&")
}

// canonicalURIPath encodes each path segment, preserving '/' separators.
// Leading slash is guaranteed.
func canonicalURIPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsURIEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// awsURIEncode implements the AWS SigV4 URI encoding rules:
// unreserved chars (A-Za-z0-9, '-', '.', '_', '~') pass through; everything
// else is %XX (uppercase hex). encodeSlash controls '/' encoding (true for
// query components, false for path segments where '/' is handled by caller).
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
