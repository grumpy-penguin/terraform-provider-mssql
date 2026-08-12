package sqlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestWithExchangeAudience(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		error bool
	}{
		{
			name: "adds audience when absent",
			in:   "https://ci.example.com/oidc/token",
			want: "https://ci.example.com/oidc/token?audience=" + "api%3A%2F%2FAzureADTokenExchange",
		},
		{
			name: "preserves existing query params",
			in:   "https://ci.example.com/oidc/token?foo=bar",
			want: "https://ci.example.com/oidc/token?audience=api%3A%2F%2FAzureADTokenExchange&foo=bar",
		},
		{
			name: "leaves an explicit audience untouched",
			in:   "https://ci.example.com/oidc/token?audience=custom",
			want: "https://ci.example.com/oidc/token?audience=custom",
		},
		{
			name:  "invalid URL",
			in:    "://not-a-url",
			error: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := withExchangeAudience(tc.in)
			if tc.error {
				if err == nil {
					t.Fatalf("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("withExchangeAudience(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseOIDCTokenResponse(t *testing.T) {
	t.Run("valid response", func(t *testing.T) {
		body, _ := json.Marshal(oidcTokenResponse{Value: "the.jwt.token"})
		got, err := parseOIDCTokenResponse(body)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "the.jwt.token" {
			t.Errorf("got %q, want %q", got, "the.jwt.token")
		}
	})

	t.Run("empty value", func(t *testing.T) {
		body, _ := json.Marshal(oidcTokenResponse{})
		if _, err := parseOIDCTokenResponse(body); err == nil {
			t.Fatal("expected an error for an empty token value")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		if _, err := parseOIDCTokenResponse([]byte("not json")); err == nil {
			t.Fatal("expected an error for malformed JSON")
		}
	})
}

func TestFetchOIDCToken(t *testing.T) {
	var gotAuth, gotAudience string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAudience = r.URL.Query().Get("audience")
		json.NewEncoder(w).Encode(oidcTokenResponse{Value: "fetched.jwt.token"})
	}))
	defer server.Close()

	token, err := fetchOIDCToken(context.Background(), server.URL, "ci-request-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "fetched.jwt.token" {
		t.Errorf("token = %q, want %q", token, "fetched.jwt.token")
	}
	if gotAuth != "Bearer ci-request-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer ci-request-token")
	}
	if gotAudience != "api://AzureADTokenExchange" {
		t.Errorf("audience query param = %q, want %q", gotAudience, "api://AzureADTokenExchange")
	}
}

func TestFetchOIDCTokenNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("nope"))
	}))
	defer server.Close()

	if _, err := fetchOIDCToken(context.Background(), server.URL, "token"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestOIDCAssertionFuncPrecedence(t *testing.T) {
	t.Run("literal token wins", func(t *testing.T) {
		fn, err := oidcAssertionFunc(AuthConfig{OIDCToken: "literal", OIDCTokenFilePath: "/should/not/be/read"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := fn(context.Background())
		if err != nil || got != "literal" {
			t.Fatalf("got (%q, %v), want (%q, nil)", got, err, "literal")
		}
	})

	t.Run("token file used when no literal token", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "oidc-token")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("from-file-token\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()

		fn, err := oidcAssertionFunc(AuthConfig{OIDCTokenFilePath: f.Name()})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := fn(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "from-file-token" {
			t.Errorf("got %q, want %q (whitespace trimmed)", got, "from-file-token")
		}
	})

	t.Run("no source configured is an error", func(t *testing.T) {
		if _, err := oidcAssertionFunc(AuthConfig{}); err == nil {
			t.Fatal("expected an error when no OIDC token source is configured")
		} else if !strings.Contains(err.Error(), "oidc_token") {
			t.Errorf("error message should mention the available options, got: %v", err)
		}
	})
}
