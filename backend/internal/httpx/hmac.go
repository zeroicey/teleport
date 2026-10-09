package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
)

// hmacSHA256 returns the HMAC-SHA256 of data under secret.
func hmacSHA256(secret, data []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	return mac.Sum(nil)
}
