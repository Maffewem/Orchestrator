package server

import "testing"

// The worked example from the AWS Signature V4 documentation (GET Object with a Range header).
func TestSignatureV4AWSExample(t *testing.T) {
	got := signatureV4("GET", "/test.txt", "", map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"x-amz-date":           "20130524T000000Z",
	}, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "20130524T000000Z")
	want := "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}
