package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/chargenet/chargenet-backend/internal/auth"
	"github.com/chargenet/chargenet-backend/internal/store"
)

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// userDTO is the public shape of a driver.
type userDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"createdAt"`
}

func toUserDTO(u *store.User) userDTO {
	return userDTO{ID: u.ID, Name: u.Name, Email: u.Email, CreatedAt: u.CreatedAt}
}

type registerRequest struct {
	WalletCurrency string `json:"walletCurrency"`
	Name           string `json:"name"`
	Email          string `json:"email"`
	Password       string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authResponse struct {
	Token string  `json:"token"`
	User  userDTO `json:"user"`
}

// registerAuthRoutes installs the mobile auth endpoints.
func registerAuthRoutes(mux *http.ServeMux, s *Server) {
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)

	authed := Chain(http.HandlerFunc(s.handleMe), RequireAuth(s.Cfg.JWTSecret))
	mux.Handle("GET /api/v1/auth/me", authed)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}

	fields := map[string]string{}
	if req.WalletCurrency != "" && !store.ValidCurrency(req.WalletCurrency) {
		fields["walletCurrency"] = "Choose MYR, SGD or THB."
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Name == "" {
		fields["name"] = "Name is required."
	}
	if !emailRE.MatchString(req.Email) {
		fields["email"] = "Enter a valid email address."
	}
	if len(req.Password) < 8 {
		fields["password"] = "Password must be at least 8 characters."
	}
	if len(fields) > 0 {
		WriteFieldError(w, "Please fix the highlighted fields.", fields)
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the account.")
		return
	}

	user, err := s.Store.CreateUser(r.Context(), req.Name, req.Email, hash, req.WalletCurrency)
	if errors.Is(err, store.ErrEmailTaken) {
		WriteError(w, http.StatusConflict, CodeEmailTaken, "That email is already registered.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the account.")
		return
	}

	token, err := auth.IssueMobileToken(s.Cfg.JWTSecret, user.ID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not create the account.")
		return
	}
	WriteJSON(w, http.StatusCreated, authResponse{Token: token, User: toUserDTO(user)})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, CodeValidation, "Malformed request body.")
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	user, err := s.Store.GetUserByEmail(r.Context(), req.Email)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !auth.CheckPassword(user.PasswordHash, req.Password)) {
		WriteError(w, http.StatusUnauthorized, CodeInvalidCredentials, "Invalid email or password.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not sign you in.")
		return
	}

	token, err := auth.IssueMobileToken(s.Cfg.JWTSecret, user.ID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not sign you in.")
		return
	}
	WriteJSON(w, http.StatusOK, authResponse{Token: token, User: toUserDTO(user)})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.Store.GetUserByID(r.Context(), UserIDFrom(r.Context()))
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "Authentication required.")
		return
	}
	if err != nil {
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not load your profile.")
		return
	}
	WriteJSON(w, http.StatusOK, toUserDTO(user))
}
