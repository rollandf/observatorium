package authentication

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc"
	"github.com/go-chi/chi"
	"golang.org/x/oauth2"
)

// contextKey to use when setting context values in the HTTP package.
type contextKey string

// String implements the Stringer interface and makes it
// nice to print contexts.
func (c contextKey) String() string {
	return "HTTP context key " + string(c)
}

const (
	state = "I love Observatorium"
	// subjectKey is the key that holds the subject in a request context.
	subjectKey contextKey = "subject"
	// tenantKey is the key that holds the tenant in a request context.
	tenantKey contextKey = "tenant"
)

// WithTenant finds the tenant from the URL parameters and adds it to the request context.
func WithTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant := chi.URLParam(r, "tenant")
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), tenantKey, tenant),
		))
	})
}

// WithTenantHeader returns a new middleware that adds the ID of the tenant to the specified header.
func WithTenantHeader(header string, tenantIDs map[string]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant := chi.URLParam(r, "tenant")
			r.Header.Add(header, tenantIDs[tenant])
			next.ServeHTTP(w, r)
		})
	}
}

// GetTenant extracts the tenant from provided context.
func GetTenant(ctx context.Context) (string, bool) {
	value := ctx.Value(tenantKey)
	tenant, ok := value.(string)

	return tenant, ok
}

// GetSubject extracts the subject from provided context.
func GetSubject(ctx context.Context) (string, bool) {
	value := ctx.Value(subjectKey)
	subject, ok := value.(string)

	return subject, ok
}

type OIDCConfig struct {
	Tenant        string
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	UsernameClaim string
}

type Middleware func(http.Handler) http.Handler

func NewOIDCHandler(configs ...OIDCConfig) (http.Handler, Middleware, error) {
	handlers := map[string]http.Handler{}
	middlewares := map[string]Middleware{}

	for _, c := range configs {
		h, m, err := newProvider(c)
		if err != nil {
			return nil, nil, err
		}

		handlers[c.Tenant] = h
		middlewares[c.Tenant] = m
	}

	r := chi.NewRouter()
	r.Mount("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := GetTenant(r.Context())
		if !ok {
			http.Error(w, "error finding tenant", http.StatusInternalServerError)
			return
		}
		h, ok := handlers[tenant]
		if !ok {
			http.Error(w, "error finding tenant", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))

	m := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenant, ok := GetTenant(r.Context())
			if !ok {
				http.Error(w, "error finding tenant", http.StatusInternalServerError)
				return
			}
			m, ok := middlewares[tenant]
			if !ok {
				http.Error(w, "error finding tenant", http.StatusUnauthorized)
				return
			}

			m(next).ServeHTTP(w, r)
		})
	}

	return r, m, nil
}

func newProvider(config OIDCConfig) (http.Handler, Middleware, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	provider, err := oidc.NewProvider(context.TODO(), config.IssuerURL)
	if err != nil {
		return nil, nil, err
	}

	var s struct {
		// What scopes does a provider support?
		//
		// See: https://openid.net/specs/openid-connect-discovery-1_0.html#ProviderMetadata
		ScopesSupported []string `json:"scopes_supported"`
	}

	if err := provider.Claims(&s); err != nil {
		return nil, nil, err
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: config.ClientID})

	oauth2Config := oauth2.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  config.RedirectURL,
		Scopes:       []string{"openid", "profile", "email"},
	}

	m := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var token string

			authorizationHeader := r.Header.Get("Authorization")
			if authorizationHeader != "" {
				authorization := strings.Split(authorizationHeader, " ")
				if len(authorization) != 2 {
					http.Error(w, "invalid Authorization header", http.StatusUnauthorized)
					return
				}

				token = authorization[1]
			} else {
				cookie, err := r.Cookie(getCookieForTenant(config.Tenant))
				if err != nil {
					http.Error(w, "failed to find token", http.StatusUnauthorized)
					return
				}
				token = cookie.Value
			}

			idToken, err := verifier.Verify(r.Context(), token)
			if err != nil {
				http.Error(w, "failed to authenticate", http.StatusBadRequest)
				return
			}

			sub := idToken.Subject
			if config.UsernameClaim != "" {
				claims := make(map[string]interface{})
				if err := idToken.Claims(&claims); err != nil {
					http.Error(w, "failed to read claims", http.StatusInternalServerError)
					return
				}
				rawUsername, ok := claims[config.UsernameClaim]
				if !ok {
					http.Error(w, "username cannot be empty", http.StatusBadRequest)
					return
				}
				username, ok := rawUsername.(string)
				if !ok || username == "" {
					http.Error(w, "invalid username claim value", http.StatusBadRequest)
					return
				}
				sub = username
			}

			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), subjectKey, sub),
			))
		})
	}

	r := chi.NewRouter()
	r.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		url := oauth2Config.AuthCodeURL(state)
		http.Redirect(w, r, url, http.StatusSeeOther)
	})

	r.Get("/callback", func(w http.ResponseWriter, r *http.Request) {
		ctx := oidc.ClientContext(r.Context(), client)

		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			desc := r.URL.Query().Get("error_description")
			http.Error(w, fmt.Sprintf("%s: %s", errMsg, desc), http.StatusBadRequest)
			return
		}

		queryCode := r.URL.Query().Get("code")
		if queryCode == "" {
			http.Error(w, "no code in request", http.StatusBadRequest)
			return
		}
		queryState := r.URL.Query().Get("state")
		if queryState != state {
			http.Error(w, "incorrect state in request", http.StatusBadRequest)
			return
		}

		token, err := oauth2Config.Exchange(ctx, queryCode)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to get token: %v", err), http.StatusInternalServerError)
			return
		}

		rawIDToken, ok := token.Extra("id_token").(string)
		if !ok {
			http.Error(w, "no id_token in token response", http.StatusInternalServerError)
			return
		}

		_, err = verifier.Verify(r.Context(), rawIDToken)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to verify ID token: %v", err), http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:    getCookieForTenant(config.Tenant),
			Value:   rawIDToken,
			Path:    "/",
			Expires: token.Expiry,
		})

		http.Redirect(w, r, "/"+config.Tenant, http.StatusFound)
	})

	return r, m, nil
}

func getCookieForTenant(tenant string) string {
	return fmt.Sprintf("observatorium_%s", tenant)
}
