// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/md5"
	"crypto/sha1"
	"fmt"
)

// BUG: Weak cryptographic hash functions — MD5 and SHA-1 are used for
// password hashing. Both are considered broken for security purposes.
// Use bcrypt, scrypt, or argon2 for password hashing, and SHA-256+
// for integrity checks.

func hashPassword(password string) string {
	h := md5.Sum([]byte(password))
	return fmt.Sprintf("%x", h)
}

func hashToken(token string) string {
	h := sha1.Sum([]byte(token))
	return fmt.Sprintf("%x", h)
}
