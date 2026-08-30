package upload

import (
	"strings"
	"testing"
	"time"
)

// TestPresignS3URL_AWSDocExample verifies the presigner against the official
// worked example in the AWS documentation:
// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html
// (GET examplebucket/test.txt, us-east-1, 86400s expiry, 20130524T000000Z)
func TestPresignS3URL_AWSDocExample(t *testing.T) {
	now, _ := time.Parse("20060102T150405Z", "20130524T000000Z")
	u := presignS3URL(
		"GET",
		"examplebucket.s3.amazonaws.com",
		"/test.txt",
		"us-east-1",
		"AKIAIOSFODNN7EXAMPLE",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"",
		86400*time.Second,
		now,
	)

	const wantSig = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.Contains(u, "X-Amz-Signature="+wantSig) {
		t.Fatalf("signature mismatch.\ngot URL: %s\nwant signature: %s", u, wantSig)
	}
	if !strings.Contains(u, "X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request") {
		t.Fatalf("credential encoding wrong: %s", u)
	}
}

func TestPresignS3URL_PathAndTokenEncoding(t *testing.T) {
	now, _ := time.Parse("20060102T150405Z", "20260101T000000Z")
	u := presignS3URL("PUT", "b.s3.ap-south-1.amazonaws.com", "products/ab cd.jpg", "ap-south-1", "AKID", "secret", "tok/en+x", 900*time.Second, now)

	if !strings.Contains(u, "/products/ab%20cd.jpg?") {
		t.Errorf("path segment not SigV4-encoded: %s", u)
	}
	if !strings.Contains(u, "X-Amz-Security-Token=tok%2Fen%2Bx") {
		t.Errorf("session token not strictly encoded: %s", u)
	}
	if !strings.Contains(u, "X-Amz-Expires=900") {
		t.Errorf("expires missing: %s", u)
	}
}
