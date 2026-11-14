package auth

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"image/png"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP parameters (RFC 6238): the defaults every authenticator app
// supports. A skew of 1 accepts the codes of the previous and next 30 s
// windows, which absorbs small clock differences between phone and server.
var validateOpts = totp.ValidateOpts{
	Period:    30,
	Skew:      1, // accept the previous and next 30-second window
	Digits:    otp.DigitsSix,
	Algorithm: otp.AlgorithmSHA1,
}

// Enrollment is a newly generated TOTP secret, ready to be scanned.
type Enrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
	QR     string `json:"qr"` // PNG as a data: URL
}

// NewEnrollment generates a new TOTP secret (160 random bits). The QR code
// encodes an otpauth:// URI that authenticator apps understand; account
// labels the entry in those apps ("alice@192.168.1.10:8443"), so that
// several users and servers can be told apart.
func NewEnrollment(account string) (Enrollment, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "cbshare",
		AccountName: account,
		Period:      uint(validateOpts.Period),
		Digits:      validateOpts.Digits,
		Algorithm:   validateOpts.Algorithm,
		SecretSize:  20,
	})
	if err != nil {
		return Enrollment{}, err
	}
	img, err := key.Image(320, 320)
	if err != nil {
		return Enrollment{}, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return Enrollment{}, err
	}
	return Enrollment{
		Secret: key.Secret(),
		URI:    key.URL(),
		QR:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// ValidateCode reports whether code is valid for secret at time t.
func ValidateCode(code, secret string, t time.Time) bool {
	_, ok := ValidateCodeStep(code, secret, t)
	return ok
}

// ValidateCodeStep checks code like ValidateCode and also returns the
// 30-second time step it belongs to, which the replay guard records.
// Every step in the tolerance window is checked, whatever matches, so the
// time taken does not depend on which one matched.
func ValidateCodeStep(code, secret string, t time.Time) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 || secret == "" {
		return 0, false
	}
	period := int64(validateOpts.Period)
	now := t.Unix() / period
	for d := -int64(validateOpts.Skew); d <= int64(validateOpts.Skew); d++ {
		want, err := totp.GenerateCodeCustom(secret, time.Unix((now+d)*period, 0), validateOpts)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 && !ok {
			step, ok = now+d, true
		}
	}
	return step, ok
}

// ReplayGuard refuses a one-time code that was already used: someone who
// sees a code being typed could otherwise reuse it during the 90 seconds
// it stays valid. It remembers, per user, the latest time step a code was
// accepted for, and accepts only later steps. It lives in memory: after a
// restart nobody is signed in, and codes from before are seconds old.
type ReplayGuard struct {
	mu   sync.Mutex
	last map[int64]int64
}

// NewReplayGuard returns an empty guard.
func NewReplayGuard() *ReplayGuard { return &ReplayGuard{last: map[int64]int64{}} }

// Use records that user signed in with a code of time step step. It
// returns false, recording nothing, when a code of that step or a later
// one was already used.
func (g *ReplayGuard) Use(user, step int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.last[user]; ok && step <= last {
		return false
	}
	g.last[user] = step
	return true
}

// Forget clears what the guard knows about user, when the user gets a new
// secret or is deleted.
func (g *ReplayGuard) Forget(user int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.last, user)
}

// Code returns the current code for secret (used in tests).
func Code(secret string, t time.Time) (string, error) {
	return totp.GenerateCodeCustom(secret, t, validateOpts)
}
