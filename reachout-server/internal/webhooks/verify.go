// Package webhooks receives Linked API events (accepted connections, replies, account
// status changes) and feeds them into the campaign state machine.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"time"
)

const (
	SignatureHeader = "linked-api-signature"
	TimestampHeader = "linked-api-signature-timestamp"

	// Tolerance is how far a signed timestamp may be from our clock, either way. It stops
	// someone replaying a captured request later.
	Tolerance = 300 * time.Second
)

var signatureRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Verify checks a Linked API webhook signature.
//
// The signature is the lowercase hex HMAC-SHA256 of "<timestamp>.<raw body>", keyed with
// the secret used as plain text (it is not hex-decoded). It must be computed over the raw
// bytes received: re-serialised JSON would not match. The comparison is constant time, and
// anything that is not a well-formed signature is rejected before any hashing.
func Verify(secret string, rawBody []byte, timestamp, signature string, now time.Time) bool {
	if secret == "" || !signatureRe.MatchString(signature) {
		return false
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	skew := now.Unix() - ts
	if skew < 0 {
		skew = -skew
	}
	if time.Duration(skew)*time.Second > Tolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	return hmac.Equal(mac.Sum(nil), got)
}

// Sign produces the signature headers for a body. Only tests and local tools need it.
func Sign(secret string, rawBody []byte, at time.Time) (timestamp, signature string) {
	timestamp = strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	return timestamp, hex.EncodeToString(mac.Sum(nil))
}
