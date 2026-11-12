package nvue

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Authenticator checks a user's password against the switch's accounts: nil when it is right,
// ErrPasswordExpired when it is right and must be changed before the account is used, and any
// other error when it is wrong or the account cannot log in.
type Authenticator func(ctx context.Context, user, password string) error

// ErrPasswordExpired is an account whose password must be changed first: Cumulus's factory user
// cumulus, whose password is expired (`chage -d 0`) so that the first login changes it (vendor
// facts B; 18 — Factory state).
var ErrPasswordExpired = errors.New("the password has expired and must be changed")

var errBadCredentials = errors.New("wrong user or password")

// The local trust path's request headers: the switch's own `nv` (D76) sends the user that ran it
// and the switch's local token, as Cumulus's `nv` reaches NVUE locally as its invoking user
// without a password.
const (
	HeaderNvUser  = "X-Dctwin-Nv-User"
	HeaderNvToken = "X-Dctwin-Nv-Token"
)

// TokenFile is where the substrate writes the local token in a factory switch (nos.Day0Dir), for
// the switch's `nv` to read.
const TokenFile = "/etc/dctwin/nvue-token"

// authCache is how long a password a switch's accounts accepted is accepted again without
// asking the switch, and how long a user's groups are kept.
const authCache = 30 * time.Second

// SetAuthenticator makes NVUE authenticate HTTP Basic credentials with a, in place of the
// single password NewServer was given (nil: that password again).
func (s *Server) SetAuthenticator(a Authenticator) {
	s.auth, s.authSet, s.authForget = a, true, nil
}

// SetLocalToken opens the local trust path: a request carrying HeaderNvUser and HeaderNvToken,
// the token equal to this one, authenticates as that user without a password, when the user
// exists in the switch and may use NVUE (root, or a member of nvshow, nvset or nvapply: Roles).
// "" closes it. The substrate writes the same token to TokenFile in the switch.
//
// The path trusts whoever can read the token: the twin's reading of Cumulus's local `nv`, which
// reaches NVUE as its invoking user (vendor facts C.2), through a token where Cumulus uses the
// local socket's credentials. A user of the switch who can read the file can name another.
func (s *Server) SetLocalToken(token string) { s.token = token }

type principalKey struct{}

// principal is who a request authenticated as, and by which path: rest (HTTP Basic) or nv (the
// local trust path).
type principal struct {
	user string
	via  string
	// accounts: the user is one of the switch's accounts (a factory switch, the local path),
	// whose groups decide what it may do; otherwise User with the twin's password, who may do
	// everything
	accounts bool
}

func principalOf(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

// authenticate is who a request is, or the status (401, 403) and why it is refused.
func (s *Server) authenticate(r *http.Request) (principal, int, string) {
	if u := r.Header.Get(HeaderNvUser); u != "" || r.Header.Get(HeaderNvToken) != "" {
		tok := r.Header.Get(HeaderNvToken)
		if s.token == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			return principal{}, http.StatusUnauthorized, "the local token is wrong (" + TokenFile + ")"
		}
		if !userName.MatchString(u) {
			return principal{}, http.StatusUnauthorized, "no such user " + strconv.Quote(u)
		}
		if _, err := s.groups(r.Context(), u); err != nil {
			return principal{}, http.StatusUnauthorized, "no such user " + u
		}
		return principal{user: u, via: "nv", accounts: true}, 0, ""
	}
	u, p, ok := r.BasicAuth()
	if !ok {
		return principal{}, http.StatusUnauthorized, "authenticate by HTTP Basic"
	}
	if s.auth == nil {
		if u != User || subtle.ConstantTimeCompare([]byte(p), []byte(s.password)) != 1 {
			return principal{}, http.StatusUnauthorized, "authenticate as " + User
		}
		return principal{user: u, via: "rest"}, 0, ""
	}
	switch err := s.auth(r.Context(), u, p); {
	case err == nil:
		return principal{user: u, via: "rest", accounts: true}, 0, ""
	case errors.Is(err, ErrPasswordExpired):
		// the twin's reading of Cumulus's forced change at first login (vendor facts B; the
		// prompt's text is UNCONFIRMED): the password is right, and the account is refused until
		// it is changed, over SSH, where PAM makes the change
		return principal{}, http.StatusForbidden, "the password of user " + u + " has expired and must be changed: log in over SSH to change it, then use the new one"
	default:
		return principal{}, http.StatusUnauthorized, "wrong user or password"
	}
}

// authorize is what an account may do, by its groups, as Cumulus's roles allow it (Roles):
// reading needs nvshow, nvset or nvapply; a change in a revision nvset or nvapply; applying
// nvapply. root and the twin's single-password user may do everything.
func (s *Server) authorize(r *http.Request, who principal) (int, string) {
	if !who.accounts || who.user == "root" {
		return 0, ""
	}
	groups, err := s.groups(r.Context(), who.user)
	if err != nil {
		return http.StatusForbidden, "user " + who.user + ": " + err.Error()
	}
	in := func(names ...string) bool {
		for _, g := range groups {
			for _, n := range names {
				if g == n {
					return true
				}
			}
		}
		return false
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.EscapedPath(), "/nvue_v1"), "/")
	apply := r.Method == http.MethodPatch && strings.HasPrefix(path, "revision/") ||
		r.Method == http.MethodPost && (path == "dctwin/save")
	switch {
	case apply:
		if !in("nvapply") {
			return http.StatusForbidden, "user " + who.user + " may not apply (not in nvapply)"
		}
	case r.Method != http.MethodGet:
		if !in("nvset", "nvapply") {
			return http.StatusForbidden, "user " + who.user + " may not change the configuration (not in nvset)"
		}
	default:
		if !in("nvshow", "nvset", "nvapply") {
			return http.StatusForbidden, "user " + who.user + " may not use NVUE (not in nvshow)"
		}
	}
	return 0, ""
}

type groupsAt struct {
	groups []string
	at     time.Time
}

// groups is a user's groups in the switch (`id -nG`), kept for authCache; an error when the user
// does not exist or the switch cannot be asked.
func (s *Server) groups(ctx context.Context, user string) ([]string, error) {
	s.groupsMu.Lock()
	g, ok := s.groupSeen[user]
	s.groupsMu.Unlock()
	if ok && time.Since(g.at) < authCache {
		return g.groups, nil
	}
	if s.run == nil {
		return nil, errors.New("the switch's accounts cannot be read")
	}
	out, err := s.run(ctx, "id", "-nG", user)
	if err != nil {
		return nil, errors.New("no such user")
	}
	gs := strings.Fields(lastLine(out))
	s.groupsMu.Lock()
	s.groupSeen[user] = groupsAt{gs, time.Now()}
	s.groupsMu.Unlock()
	return gs, nil
}

// ShadowAuthenticator checks a password against the switch's /etc/shadow, read through run, as
// PAM's pam_unix checks it for Cumulus's NVUE: a SHA-512 crypt hash ($6$, what NVUE and the
// image's chpasswd write) is checked here; any other (yescrypt, $y$, which Debian bookworm's PAM
// writes when a user changes their own password over SSH) by the switch's libcrypt, through
// python3's crypt module, the password then passing on that process's command line inside the
// switch. A locked account ("!", "*") never authenticates. A right password whose account must
// change it (shadow's last change 0, `chage -d 0`, or past its maximum age) is
// ErrPasswordExpired. A success is kept for authCache, so a client's requests do not each ask
// the switch.
func ShadowAuthenticator(run Runner) Authenticator {
	a, _ := shadowAuthenticator(run)
	return a
}

// shadowAuthenticator is ShadowAuthenticator and what forgets its successes: the Server calls it
// after every apply, so a password NVUE changed is not accepted from the cache.
func shadowAuthenticator(run Runner) (Authenticator, func()) {
	var mu sync.Mutex
	ok := map[string]time.Time{}
	forget := func() {
		mu.Lock()
		clear(ok)
		mu.Unlock()
	}
	return func(ctx context.Context, user, password string) error {
		if !userName.MatchString(user) {
			return errBadCredentials
		}
		h := sha256.Sum256([]byte(user + "\x00" + password))
		key := hex.EncodeToString(h[:])
		mu.Lock()
		at, seen := ok[key]
		mu.Unlock()
		if seen && time.Since(at) < authCache {
			return nil
		}
		out, err := run(ctx, "getent", "shadow", user)
		if err != nil {
			return errBadCredentials
		}
		var f []string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, user+":") {
				f = strings.Split(strings.TrimSpace(l), ":")
			}
		}
		if len(f) < 3 {
			return errBadCredentials
		}
		hash := f[1]
		if hash == "" || hash[0] == '!' || hash[0] == '*' {
			return errBadCredentials
		}
		match, known := verifySHA512Crypt(password, hash)
		if !known {
			o, err := run(ctx, "python3", "-W", "ignore", "-c", "import crypt, sys; print(crypt.crypt(sys.argv[1], sys.argv[2]))", password, hash)
			match = err == nil && subtle.ConstantTimeCompare([]byte(lastLine(o)), []byte(hash)) == 1
		}
		if !match {
			return errBadCredentials
		}
		if expired(f) {
			return ErrPasswordExpired
		}
		mu.Lock()
		ok[key] = time.Now()
		mu.Unlock()
		return nil
	}, forget
}

// expired is whether a shadow entry's password must be changed: its last change is 0 (shadow(5):
// "the user should change their password the next time they log in"), or it is older than its
// maximum age.
func expired(f []string) bool {
	if f[2] == "0" {
		return true
	}
	if len(f) > 4 && f[2] != "" && f[4] != "" {
		last, err1 := strconv.Atoi(f[2])
		max, err2 := strconv.Atoi(f[4])
		if err1 == nil && err2 == nil && max >= 0 && max < 99999 {
			today := int(time.Now().Unix() / 86400)
			return today > last+max
		}
	}
	return false
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
