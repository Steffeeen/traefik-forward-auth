package tfa

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thomseddon/traefik-forward-auth/internal/provider"
)

// Request Validation

var users = make(map[uuid.UUID]*UserEntry)

type UserEntry struct {
	User    *provider.User
	AddedAt time.Time
}

var started = false

func cleanUsers() {
	for userUUID, user := range users {
		if time.Since(user.AddedAt).Hours() > 1 {
			delete(users, userUUID)
		}
	}
	time.Sleep(5 * time.Minute)
}

func ensureUser(user *provider.User) {
	if !started {
		go cleanUsers()
		started = true
	}

	if _, ok := users[user.UUID]; !ok {
		users[user.UUID] = &UserEntry{
			User:    user,
			AddedAt: time.Now(),
		}
	}
}

// ValidateCookie verifies that a cookie matches the expected format of:
// Cookie = hash(secret, cookie domain, userUUID, expires)|expires|userUUID
func ValidateCookie(r *http.Request, c *http.Cookie) (*provider.User, error) {
	parts := strings.Split(c.Value, "|")

	if len(parts) != 3 {
		return nil, errors.New("Invalid cookie format")
	}

	mac, err := base64.URLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("Unable to decode cookie mac")
	}

	var userUUID uuid.UUID

	err = userUUID.UnmarshalText([]byte(parts[2]))
	if err != nil {
		return nil, err
	}

	userEntry := users[userUUID]
	if userEntry == nil {
		return nil, errors.New("user is unknown")
	}
	user := userEntry.User

	expectedSignature, _ := cookieSignature(r, user, parts[1])
	expected, err := base64.URLEncoding.DecodeString(expectedSignature)
	if err != nil {
		return nil, errors.New("Unable to generate mac")
	}

	// Valid token?
	if !hmac.Equal(mac, expected) {
		return nil, errors.New("Invalid cookie mac")
	}

	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, errors.New("Unable to parse cookie expiry")
	}

	// Has it expired?
	if time.Unix(expires, 0).Before(time.Now()) {
		return nil, errors.New("Cookie has expired")
	}

	// Looks valid
	return user, nil
}

// ValidateUser checks if the given email address matches either a whitelisted
// email address, as defined by the "whitelist" config parameter. Or is part of
// a permitted domain, as defined by the "domains" config parameter
func ValidateUser(user *provider.User, ruleName string) bool {
	// Use global config by default
	whitelist := config.Whitelist
	domains := config.Domains
	allowedRoles := config.AllowedRoles

	if rule, ok := config.Rules[ruleName]; ok {
		// Override with rule config if found
		if len(rule.Whitelist) > 0 || len(rule.Domains) > 0 {
			whitelist = rule.Whitelist
			domains = rule.Domains
		}

		if len(rule.AllowedRoles) > 0 {
			allowedRoles = rule.AllowedRoles
		}
	}

	// Do we have any validation to perform?
	if len(whitelist) == 0 && len(domains) == 0 && len(allowedRoles) == 0 {
		return true
	}

	// Email whitelist validation
	if len(whitelist) > 0 {
		if ValidateWhitelist(user.Email, whitelist) {
			return true
		}
	}

	// Domain validation
	if len(domains) > 0 && ValidateDomains(user.Email, domains) {
		return true
	}

	// allowed rules validation
	if len(allowedRoles) > 0 && ValidateRoles(user, allowedRoles) {
		return true
	}

	return false
}

func ValidateRoles(user *provider.User, allowedRoles CommaSeparatedList) bool {
	log.Debugf("User %s has the following rules: %v", user.Name, user.Roles)
	for _, allowedRole := range allowedRoles {
		for _, userRole := range user.Roles {
			if allowedRole == userRole {
				return true
			}
		}
	}
	return false
}

// ValidateWhitelist checks if the email is in whitelist
func ValidateWhitelist(email string, whitelist CommaSeparatedList) bool {
	for _, whitelist := range whitelist {
		if email == whitelist {
			return true
		}
	}
	return false
}

// ValidateDomains checks if the email matches a whitelisted domain
func ValidateDomains(email string, domains CommaSeparatedList) bool {
	parts := strings.Split(email, "@")
	if len(parts) < 2 {
		return false
	}
	for _, domain := range domains {
		if domain == parts[1] {
			return true
		}
	}
	return false
}

// Utility methods

// Get the redirect base
func redirectBase(r *http.Request) string {
	return fmt.Sprintf("%s://%s", r.Header.Get("X-Forwarded-Proto"), r.Host)
}

// Return url
func returnUrl(r *http.Request) string {
	return fmt.Sprintf("%s%s", redirectBase(r), r.URL.Path)
}

// Get oauth redirect uri
func redirectUri(r *http.Request) string {
	if use, _ := useAuthDomain(r); use {
		p := r.Header.Get("X-Forwarded-Proto")
		return fmt.Sprintf("%s://%s%s", p, config.AuthHost, config.Path)
	}

	return fmt.Sprintf("%s%s", redirectBase(r), config.Path)
}

// Should we use auth host + what it is
func useAuthDomain(r *http.Request) (bool, string) {
	if config.AuthHost == "" {
		return false, ""
	}

	// Does the request match a given cookie domain?
	reqMatch, reqHost := matchCookieDomains(r.Host)

	// Do any of the auth hosts match a cookie domain?
	authMatch, authHost := matchCookieDomains(config.AuthHost)

	// We need both to match the same domain
	return reqMatch && authMatch && reqHost == authHost, reqHost
}

// Cookie methods

// MakeCookie creates an auth cookie
func MakeCookie(r *http.Request, user *provider.User) (*http.Cookie, error) {
	expires := cookieExpiry()
	mac, err := cookieSignature(r, user, fmt.Sprintf("%d", expires.Unix()))
	if err != nil {
		return nil, err
	}
	value := fmt.Sprintf("%s|%d|%s", mac, expires.Unix(), user.UUID)

	return &http.Cookie{
		Name:     config.CookieName,
		Value:    value,
		Path:     "/",
		Domain:   cookieDomain(r),
		HttpOnly: true,
		Secure:   !config.InsecureCookie,
		Expires:  expires,
	}, nil
}

// ClearCookie clears the auth cookie
func ClearCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     config.CookieName,
		Value:    "",
		Path:     "/",
		Domain:   cookieDomain(r),
		HttpOnly: true,
		Secure:   !config.InsecureCookie,
		Expires:  time.Now().Local().Add(time.Hour * -1),
	}
}

func buildCSRFCookieName(nonce string) string {
	return config.CSRFCookieName + "_" + nonce[:6]
}

// MakeCSRFCookie makes a csrf cookie (used during login only)
//
// Note, CSRF cookies live shorter than auth cookies, a fixed 1h.
// That's because some CSRF cookies may belong to auth flows that don't complete
// and thus may not get cleared by ClearCookie.
func MakeCSRFCookie(r *http.Request, nonce string) *http.Cookie {
	return &http.Cookie{
		Name:     buildCSRFCookieName(nonce),
		Value:    nonce,
		Path:     "/",
		Domain:   csrfCookieDomain(r),
		HttpOnly: true,
		Secure:   !config.InsecureCookie,
		Expires:  time.Now().Local().Add(time.Hour * 1),
	}
}

// ClearCSRFCookie makes an expired csrf cookie to clear csrf cookie
func ClearCSRFCookie(r *http.Request, c *http.Cookie) *http.Cookie {
	return &http.Cookie{
		Name:     c.Name,
		Value:    "",
		Path:     "/",
		Domain:   csrfCookieDomain(r),
		HttpOnly: true,
		Secure:   !config.InsecureCookie,
		Expires:  time.Now().Local().Add(time.Hour * -1),
	}
}

// FindCSRFCookie extracts the CSRF cookie from the request based on state.
func FindCSRFCookie(r *http.Request, state string) (c *http.Cookie, err error) {
	// Check for CSRF cookie
	return r.Cookie(buildCSRFCookieName(state))
}

// ValidateCSRFCookie validates the csrf cookie against state
func ValidateCSRFCookie(c *http.Cookie, state string) (valid bool, provider string, redirect string, err error) {
	if len(c.Value) != 32 {
		return false, "", "", errors.New("Invalid CSRF cookie value")
	}

	// Check nonce match
	if c.Value != state[:32] {
		return false, "", "", errors.New("CSRF cookie does not match state")
	}

	// Extract provider
	params := state[33:]
	split := strings.Index(params, ":")
	if split == -1 {
		return false, "", "", errors.New("Invalid CSRF state format")
	}

	// Valid, return provider and redirect
	return true, params[:split], params[split+1:], nil
}

// MakeState generates a state value
func MakeState(r *http.Request, p provider.Provider, nonce string) string {
	return fmt.Sprintf("%s:%s:%s", nonce, p.Name(), returnUrl(r))
}

// ValidateState checks whether the state is of right length.
func ValidateState(state string) error {
	if len(state) < 34 {
		return errors.New("Invalid CSRF state value")
	}
	return nil
}

// Nonce generates a random nonce
func Nonce() (error, string) {
	nonce := make([]byte, 16)
	_, err := rand.Read(nonce)
	if err != nil {
		return err, ""
	}

	return nil, fmt.Sprintf("%x", nonce)
}

// Cookie domain
func cookieDomain(r *http.Request) string {
	// Check if any of the given cookie domains matches
	_, domain := matchCookieDomains(r.Host)
	return domain
}

// Cookie domain
func csrfCookieDomain(r *http.Request) string {
	var host string
	if use, domain := useAuthDomain(r); use {
		host = domain
	} else {
		host = r.Host
	}

	// Remove port
	p := strings.Split(host, ":")
	return p[0]
}

// Return matching cookie domain if exists
func matchCookieDomains(domain string) (bool, string) {
	// Remove port
	p := strings.Split(domain, ":")

	for _, d := range config.CookieDomains {
		if d.Match(p[0]) {
			return true, d.Domain
		}
	}

	return false, p[0]
}

// Create cookie hmac
func cookieSignature(r *http.Request, user *provider.User, expires string) (string, error) {
	hash := hmac.New(sha256.New, config.Secret)
	hash.Write([]byte(cookieDomain(r)))
	uuidBytes, err := user.UUID.MarshalBinary()
	if err != nil {
		return "", errors.New("unable to convert UUID to bytes")
	}
	hash.Write(uuidBytes)
	hash.Write([]byte(expires))
	return base64.URLEncoding.EncodeToString(hash.Sum(nil)), nil
}

// Get cookie expiry
func cookieExpiry() time.Time {
	return time.Now().Local().Add(config.Lifetime)
}

// CookieDomain holds cookie domain info
type CookieDomain struct {
	Domain       string
	DomainLen    int
	SubDomain    string
	SubDomainLen int
}

// NewCookieDomain creates a new CookieDomain from the given domain string
func NewCookieDomain(domain string) *CookieDomain {
	return &CookieDomain{
		Domain:       domain,
		DomainLen:    len(domain),
		SubDomain:    fmt.Sprintf(".%s", domain),
		SubDomainLen: len(domain) + 1,
	}
}

// Match checks if the given host matches this CookieDomain
func (c *CookieDomain) Match(host string) bool {
	// Exact domain match?
	if host == c.Domain {
		return true
	}

	// Subdomain match?
	if len(host) >= c.SubDomainLen && host[len(host)-c.SubDomainLen:] == c.SubDomain {
		return true
	}

	return false
}

// UnmarshalFlag converts a string to a CookieDomain
func (c *CookieDomain) UnmarshalFlag(value string) error {
	*c = *NewCookieDomain(value)
	return nil
}

// MarshalFlag converts a CookieDomain to a string
func (c *CookieDomain) MarshalFlag() (string, error) {
	return c.Domain, nil
}

// CookieDomains provides legacy sypport for comma separated list of cookie domains
type CookieDomains []CookieDomain

// UnmarshalFlag converts a comma separated list of cookie domains to an array
// of CookieDomains
func (c *CookieDomains) UnmarshalFlag(value string) error {
	if len(value) > 0 {
		for _, d := range strings.Split(value, ",") {
			cookieDomain := NewCookieDomain(d)
			*c = append(*c, *cookieDomain)
		}
	}
	return nil
}

// MarshalFlag converts an array of CookieDomain to a comma seperated list
func (c *CookieDomains) MarshalFlag() (string, error) {
	var domains []string
	for _, d := range *c {
		domains = append(domains, d.Domain)
	}
	return strings.Join(domains, ","), nil
}
