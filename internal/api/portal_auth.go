package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chargenet/chargenet-backend/internal/auth"
	"github.com/chargenet/chargenet-backend/internal/store"
)

// The portal session uses an HttpOnly JWT cookie with the `portal` audience so
// browser scripts cannot read it.
const (
	portalCookieName = "cn_portal"
	portalTokenTTL   = 12 * time.Hour
)

// portalPartyRE / portalCountryRE are the registration validators
// for partner identity and bank details.
var (
	portalPartyRE   = regexp.MustCompile(`^[A-Z0-9]{3}$`)
	portalCountryRE = regexp.MustCompile(`^[A-Z]{2}$`)
	portalAccountRE = regexp.MustCompile(`^[0-9]{8,20}$`)
)

// portalUserDTO is what the console renders as "who am I".
type portalUserDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Role  string `json:"role"`
	CPOID string `json:"cpoId,omitempty"`
}

func toPortalUserDTO(u *store.PortalUser) portalUserDTO {
	return portalUserDTO{ID: u.ID, Name: u.Name, Email: u.Email, Role: u.Role, CPOID: u.CPOID}
}

// portalLimiter is a fixed-window failure counter for logins: at most max
// failures per window and key, after which the key is blocked until the window
// drains. It is deliberately in-process — one serial connection to Postgres per
// failed attempt is not worth the latency it would add to a brute-force probe.
type portalLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newPortalLimiter(max int, window time.Duration) *portalLimiter {
	return &portalLimiter{max: max, window: window, hits: map[string][]time.Time{}}
}

// blocked reports whether key has spent its budget inside the current window.
func (l *portalLimiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key, now)) >= l.max
}

// fail records one failed attempt for key.
func (l *portalLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.recent(key, now), now)
}

// clear forgets a key's failures (a successful login).
func (l *portalLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// recent returns the failures still inside the window, pruning the rest.
func (l *portalLimiter) recent(key string, now time.Time) []time.Time {
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

// setPortalCookie writes the session cookie. Secure is set only when the public
// base URL is https, so a plain-http local demo still works.
func (s *Server) setPortalCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     portalCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(strings.ToLower(s.Cfg.PublicBaseURL), "https://"),
		MaxAge:   int(portalTokenTTL / time.Second),
	})
}

// clearPortalCookie expires the session cookie (logout).
func (s *Server) clearPortalCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     portalCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   strings.HasPrefix(strings.ToLower(s.Cfg.PublicBaseURL), "https://"),
		MaxAge:   -1,
	})
}

// portalClaims validates the session cookie and returns its claims.
func (s *Server) portalClaims(r *http.Request) (*auth.Claims, error) {
	cookie, err := r.Cookie(portalCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, auth.ErrInvalidToken
	}
	return auth.ParseToken(s.Cfg.JWTSecret, cookie.Value, auth.AudPortal)
}

// newTokenA mints a 32-byte hex registration token.
func newTokenA() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// clientIP is the remote host, used as part of the login-rate-limit key. A
// forwarded header is not trusted: the backend may sit behind any proxy.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// registerPortalAuthRoutes installs the portal's login/logout/me/register
// endpoints.
func registerPortalAuthRoutes(mux *http.ServeMux, s *Server) {
	mux.HandleFunc("POST /admin/api/login", s.handlePortalLogin)
	mux.HandleFunc("POST /admin/api/logout", s.handlePortalLogout)
	mux.HandleFunc("POST /admin/api/register", s.handlePortalRegister)
	mux.Handle("GET /admin/api/me", Chain(http.HandlerFunc(s.handlePortalMe), s.RequirePortal()))
}

// RequirePortal authenticates a portal request from its cookie and injects the
// caller's user id, role and CPO id. With one or more roles it also checks the
// role, so a CPO-role console can never reach a super-admin handler — the
// handler then scopes its own queries by CPOIDFrom, never by a client-supplied
// id.
func (s *Server) RequirePortal(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := s.portalClaims(r)
			if err != nil {
				WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "Sign in to continue.")
				return
			}
			if len(roles) > 0 && !containsString(roles, claims.Role) {
				WriteError(w, http.StatusForbidden, CodeForbidden, "Your account cannot do that.")
				return
			}
			ctx := context.WithValue(r.Context(), ctxUserID, claims.Subject)
			ctx = context.WithValue(ctx, ctxRole, claims.Role)
			ctx = context.WithValue(ctx, ctxCPOID, claims.CPOID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// loginRequest is the `POST /admin/api/login` body.
type portalLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handlePortalLogin verifies a password and starts a session. Five failures per
// five minutes per IP+email answer 429; a success clears them.
func (s *Server) handlePortalLogin(w http.ResponseWriter, r *http.Request) {
	var body portalLoginRequest
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	now := time.Now()
	key := clientIP(r) + "|" + email
	if s.portalLimiter.blocked(key, now) {
		WriteError(w, http.StatusTooManyRequests, CodeRateLimited, "Too many attempts. Try again in a few minutes.")
		return
	}

	user, err := s.Store.GetPortalUserByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !auth.CheckPassword(user.PasswordHash, body.Password)) {
		s.portalLimiter.fail(key, now)
		WriteError(w, http.StatusUnauthorized, CodeInvalidCredentials, "Invalid email or password.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not sign you in.")
		return
	}
	s.portalLimiter.clear(key)

	token, err := auth.IssuePortalToken(s.Cfg.JWTSecret, user.ID, user.Role, user.CPOID, portalTokenTTL)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not sign you in.")
		return
	}
	s.setPortalCookie(w, token)
	WriteJSON(w, http.StatusOK, toPortalUserDTO(user))
}

// handlePortalLogout expires the cookie. The token itself is stateless, so
// clearing it on the client is the whole logout.
func (s *Server) handlePortalLogout(w http.ResponseWriter, _ *http.Request) {
	s.clearPortalCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// handlePortalMe re-reads the signed-in user, which the console calls on load to
// decide the landing page from the role.
func (s *Server) handlePortalMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.Store.GetPortalUserByID(r.Context(), UserIDFrom(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "Sign in to continue.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your profile.")
		return
	}
	WriteJSON(w, http.StatusOK, toPortalUserDTO(user))
}

// portalRegisterRequest is the `POST /admin/api/register` body.
type portalRegisterRequest struct {
	CompanyName  string `json:"companyName"`
	PartyID      string `json:"partyId"`
	CountryCode  string `json:"countryCode"`
	BaseURL      string `json:"baseUrl"`
	ContactEmail string `json:"contactEmail"`
	ContactName  string `json:"contactName"`
	Password     string `json:"password"`
	Bank         struct {
		BankName      string `json:"bankName"`
		AccountHolder string `json:"accountHolder"`
		AccountNo     string `json:"accountNo"`
	} `json:"bank"`
	PayoutCycle string `json:"payoutCycle"`
}

// handlePortalRegister self-registers a CPO: it validates the the portal
// rules, creates the PENDING partner and its login in one transaction, and signs
// the user in. The full account number is stored; only the last four digits are
// ever read back.
func (s *Server) handlePortalRegister(w http.ResponseWriter, r *http.Request) {
	var body portalRegisterRequest
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}

	body.CompanyName = strings.TrimSpace(body.CompanyName)
	body.PartyID = strings.ToUpper(strings.TrimSpace(body.PartyID))
	body.CountryCode = strings.ToUpper(strings.TrimSpace(body.CountryCode))
	body.ContactEmail = strings.ToLower(strings.TrimSpace(body.ContactEmail))
	body.PayoutCycle = strings.ToUpper(strings.TrimSpace(body.PayoutCycle))

	fields := map[string]string{}
	if body.CompanyName == "" {
		fields["companyName"] = "Company name is required."
	}
	if !portalPartyRE.MatchString(body.PartyID) {
		fields["partyId"] = "Party id must be exactly 3 upper-case letters or digits."
	}
	if !portalCountryRE.MatchString(body.CountryCode) || (body.CountryCode != "MY" && body.CountryCode != "SG" && body.CountryCode != "TH") {
		fields["countryCode"] = "Choose MY, SG or TH."
	}
	if !emailRE.MatchString(body.ContactEmail) {
		fields["contactEmail"] = "Enter a valid email address."
	}
	if strings.TrimSpace(body.ContactName) == "" {
		fields["contactName"] = "Contact name is required."
	}
	if len(body.Password) < 8 {
		fields["password"] = "Password must be at least 8 characters."
	}
	if !portalAccountRE.MatchString(strings.TrimSpace(body.Bank.AccountNo)) {
		fields["bank.accountNo"] = "Account number must be 8-20 digits."
	}
	if !containsString(store.PayoutCycles, body.PayoutCycle) {
		fields["payoutCycle"] = "Payout cycle must be WEEKLY or MONTHLY."
	}
	if len(fields) > 0 {
		WriteFieldError(w, "Please fix the highlighted fields.", fields)
		return
	}

	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the account.")
		return
	}
	tokenA, err := newTokenA()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the account.")
		return
	}

	profile, user, err := s.Store.RegisterCPO(r.Context(), store.PortalRegisterInput{
		CompanyName:       body.CompanyName,
		Slug:              slugify(body.CompanyName),
		CountryCode:       body.CountryCode,
		PartyID:           body.PartyID,
		BaseURL:           strings.TrimSpace(body.BaseURL),
		ContactEmail:      body.ContactEmail,
		ContactName:       body.ContactName,
		Email:             body.ContactEmail,
		PasswordHash:      hash,
		BankName:          strings.TrimSpace(body.Bank.BankName),
		BankAccountHolder: strings.TrimSpace(body.Bank.AccountHolder),
		BankAccountNo:     strings.TrimSpace(body.Bank.AccountNo),
		PayoutCycle:       body.PayoutCycle,
		TokenA:            tokenA,
		PlatformFeePct:    s.Cfg.PlatformFeePct,
	})
	switch {
	case errors.Is(err, store.ErrSlugTaken):
		WriteFieldError(w, "Please fix the highlighted fields.",
			map[string]string{"companyName": "A company with a similar name is already registered."})
		return
	case errors.Is(err, store.ErrPartyIDTaken):
		WriteFieldError(w, "Please fix the highlighted fields.",
			map[string]string{"partyId": "That party id is already registered."})
		return
	case errors.Is(err, store.ErrEmailTaken):
		WriteFieldError(w, "Please fix the highlighted fields.",
			map[string]string{"contactEmail": "That email is already registered."})
		return
	case err != nil:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the account.")
		return
	}

	token, err := auth.IssuePortalToken(s.Cfg.JWTSecret, user.ID, user.Role, user.CPOID, portalTokenTTL)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the account.")
		return
	}
	s.setPortalCookie(w, token)
	WriteJSON(w, http.StatusCreated, map[string]any{"user": toPortalUserDTO(user), "cpo": profile})
}

// slugify derives a partner slug from a company name: lower
// case, alphanumerics only, at most 32 characters. Uniqueness is the database's
// job (cpo_partners.slug is unique), and a collision comes back as ErrSlugTaken.
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	slug := b.String()
	if slug == "" {
		slug = "cpo"
	}
	if len(slug) > 32 {
		slug = slug[:32]
	}
	return slug
}
