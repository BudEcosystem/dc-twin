package nvue

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"fmt"
	"strconv"
	"strings"
)

// HashPassword is a password as NVUE stores it for `nv set system aaa user <name> password
// <plain>` (18 — The first push): hashed, SHA-512 crypt ($6$, the scheme Cumulus Linux's
// /etc/shadow holds and `hashed-password` takes), with a random salt. The plain password is
// never kept in the tree.
func HashPassword(plain string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	for i := range salt {
		salt[i] = cryptAlphabet[salt[i]&0x3f]
	}
	return sha512Crypt(plain, string(salt), 5000, false)
}

// hashStable is HashPassword with a salt derived from the user and the password, for a plain
// password Render meets in a tree that did not come through the REST API (a test's, a startup
// file's): the same tree renders the same files.
func hashStable(user, plain string) string {
	h := sha256.Sum256([]byte("dctwin-nvue-salt/" + user + "/" + plain))
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = cryptAlphabet[int(h[i])%64]
	}
	return sha512Crypt(plain, string(salt), 5000, false)
}

// verifySHA512Crypt checks a password against a $6$ hash; ok is false when the hash is not
// SHA-512 crypt (yescrypt, $y$, which Debian bookworm's PAM writes when a user changes their own
// password, is checked in the switch, ShadowAuthenticator).
func verifySHA512Crypt(plain, hash string) (match, ok bool) {
	rest, found := strings.CutPrefix(hash, "$6$")
	if !found {
		return false, false
	}
	rounds, custom := 5000, false
	if r, after, found := strings.Cut(rest, "$"); found && strings.HasPrefix(r, "rounds=") {
		n, err := strconv.Atoi(strings.TrimPrefix(r, "rounds="))
		if err != nil {
			return false, false
		}
		rounds, custom, rest = n, true, after
	}
	salt, _, found := strings.Cut(rest, "$")
	if !found {
		return false, false
	}
	got := sha512Crypt(plain, salt, rounds, custom)
	return subtle.ConstantTimeCompare([]byte(got), []byte(hash)) == 1, true
}

const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// sha512Crypt is Ulrich Drepper's SHA-512 crypt ("Unix crypt using SHA-256 and SHA-512",
// https://www.akkadia.org/drepper/SHA-crypt.txt), as glibc and libxcrypt implement it.
func sha512Crypt(key, salt string, rounds int, custom bool) string {
	if len(salt) > 16 {
		salt = salt[:16]
	}
	if rounds < 1000 {
		rounds = 1000
	}
	if rounds > 999999999 {
		rounds = 999999999
	}
	k, s := []byte(key), []byte(salt)
	b := sha512.New()
	b.Write(k)
	b.Write(s)
	b.Write(k)
	bd := b.Sum(nil)

	a := sha512.New()
	a.Write(k)
	a.Write(s)
	n := len(k)
	for ; n > 64; n -= 64 {
		a.Write(bd)
	}
	a.Write(bd[:n])
	for n := len(k); n > 0; n >>= 1 {
		if n&1 != 0 {
			a.Write(bd)
		} else {
			a.Write(k)
		}
	}
	ad := a.Sum(nil)

	dp := sha512.New()
	for i := 0; i < len(k); i++ {
		dp.Write(k)
	}
	dpd := dp.Sum(nil)
	p := make([]byte, 0, len(k))
	for n := len(k); n > 0; n -= 64 {
		if n >= 64 {
			p = append(p, dpd...)
		} else {
			p = append(p, dpd[:n]...)
		}
	}

	ds := sha512.New()
	for i := 0; i < 16+int(ad[0]); i++ {
		ds.Write(s)
	}
	dsd := ds.Sum(nil)
	sb := make([]byte, 0, len(s))
	for n := len(s); n > 0; n -= 64 {
		if n >= 64 {
			sb = append(sb, dsd...)
		} else {
			sb = append(sb, dsd[:n]...)
		}
	}

	prev := ad
	for i := 0; i < rounds; i++ {
		c := sha512.New()
		if i&1 != 0 {
			c.Write(p)
		} else {
			c.Write(prev)
		}
		if i%3 != 0 {
			c.Write(sb)
		}
		if i%7 != 0 {
			c.Write(p)
		}
		if i&1 != 0 {
			c.Write(prev)
		} else {
			c.Write(p)
		}
		prev = c.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$")
	if custom {
		fmt.Fprintf(&out, "rounds=%d$", rounds)
	}
	out.WriteString(salt)
	out.WriteByte('$')
	enc := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for ; n > 0; n-- {
			out.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	r := prev
	for _, t := range [][3]int{{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48},
		{28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35},
		{15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41}} {
		enc(r[t[0]], r[t[1]], r[t[2]], 4)
	}
	enc(0, 0, r[63], 2)
	return out.String()
}
