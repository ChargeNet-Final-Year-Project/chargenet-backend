package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chargenet/chargenet-backend/internal/api"
	"github.com/chargenet/chargenet-backend/internal/config"
	"github.com/chargenet/chargenet-backend/internal/store"
	"github.com/chargenet/chargenet-backend/internal/testutil"
)

func newTestRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	cfg := config.Config{JWTSecret: "test-secret", MinStartBalance: 10}
	return api.NewRouter(cfg, st), st
}

func doJSON(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

type authResp struct {
	Token string `json:"token"`
	User  struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`
}

type errResp struct {
	Error struct {
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	} `json:"error"`
}

func TestRegisterLoginMe(t *testing.T) {
	h, _ := newTestRouter(t)

	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"name": "Aisyah", "email": "Aisyah@Example.com", "password": "Secret123",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	reg := decode[authResp](t, rec)
	if reg.Token == "" || reg.User.ID == "" {
		t.Fatalf("register response missing token/user: %+v", reg)
	}
	if reg.User.Email != "aisyah@example.com" {
		t.Fatalf("email not normalised: %q", reg.User.Email)
	}

	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": "AISYAH@example.com", "password": "Secret123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	login := decode[authResp](t, rec)

	rec = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", login.Token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	me := decode[struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}](t, rec)
	if me.ID != login.User.ID {
		t.Fatalf("me id = %q, want %q", me.ID, login.User.ID)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h, _ := newTestRouter(t)
	doJSON(t, h, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"name": "Bala", "email": "bala@example.com", "password": "Secret123",
	})

	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": "bala@example.com", "password": "nope",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if body := decode[errResp](t, rec); body.Error.Code != "INVALID_CREDENTIALS" {
		t.Fatalf("code = %q, want INVALID_CREDENTIALS", body.Error.Code)
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	h, _ := newTestRouter(t)
	payload := map[string]string{"name": "Chen", "email": "chen@example.com", "password": "Secret123"}
	if rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/register", "", payload); rec.Code != http.StatusCreated {
		t.Fatalf("first register status = %d", rec.Code)
	}
	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/register", "", payload)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", rec.Code)
	}
	if body := decode[errResp](t, rec); body.Error.Code != "EMAIL_TAKEN" {
		t.Fatalf("code = %q, want EMAIL_TAKEN", body.Error.Code)
	}
}

func TestRegisterValidation(t *testing.T) {
	h, _ := newTestRouter(t)
	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"name": "", "email": "bad", "password": "short",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := decode[errResp](t, rec)
	if body.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("code = %q, want VALIDATION_ERROR", body.Error.Code)
	}
	for _, field := range []string{"name", "email", "password"} {
		if _, ok := body.Error.Fields[field]; !ok {
			t.Fatalf("expected validation error for %q, got %v", field, body.Error.Fields)
		}
	}
}

func TestMeRequiresValidToken(t *testing.T) {
	h, _ := newTestRouter(t)

	for _, token := range []string{"", "garbage.token.value"} {
		rec := doJSON(t, h, http.MethodGet, "/api/v1/auth/me", token, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, rec.Code)
		}
		if body := decode[errResp](t, rec); body.Error.Code != "UNAUTHORIZED" {
			t.Fatalf("token %q: code = %q, want UNAUTHORIZED", token, body.Error.Code)
		}
	}
}

func TestSeedUsersDemoLogin(t *testing.T) {
	ctx := context.Background()
	h, st := newTestRouter(t)
	if err := st.SeedUsers(ctx); err != nil {
		t.Fatalf("SeedUsers: %v", err)
	}

	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": store.DemoEmail, "password": store.DemoPassword,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("demo login status = %d (%s)", rec.Code, rec.Body)
	}

	if err := st.SeedUsers(ctx); err != nil {
		t.Fatalf("second SeedUsers: %v", err)
	}
	n, err := st.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 2 { // demo driver + sandbox driver
		t.Fatalf("user count = %d, want 2", n)
	}
}
