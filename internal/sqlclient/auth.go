package sqlclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// azureADTokenExchangeAudience is the fixed audience Azure AD expects on an
// OIDC token presented as a workload identity federation client assertion.
// CI-issued OIDC tokens (e.g. GitHub Actions' ID token) must be requested
// with this audience or Azure AD will reject the federated credential
// exchange.
const azureADTokenExchangeAudience = "api://AzureADTokenExchange"

// databaseScope is the resource scope Azure SQL Database expects an access
// token to be issued for.
const databaseScope = "https://database.windows.net/.default"

// buildCredential constructs the azcore.TokenCredential used to authenticate
// to Azure SQL Database, based on cfg. Two mutually exclusive modes are
// supported:
//
//   - Client secret (default): a plain Azure AD App Registration secret.
//   - OIDC (cfg.UseOIDC): workload identity federation, exchanging a
//     CI-issued OIDC token for an Azure AD access token via a federated
//     credential configured on the App Registration. No secret is needed
//     or used in this mode.
func buildCredential(cfg AuthConfig) (azcore.TokenCredential, error) {
	if cfg.TenantID == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("tenant_id and client_id are required")
	}

	if cfg.UseOIDC {
		assertion, err := oidcAssertionFunc(cfg)
		if err != nil {
			return nil, err
		}
		cred, err := azidentity.NewClientAssertionCredential(cfg.TenantID, cfg.ClientID, assertion, nil)
		if err != nil {
			return nil, fmt.Errorf("building OIDC (client assertion) credential: %w", err)
		}
		return cred, nil
	}

	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("client_secret is required unless use_oidc is enabled")
	}
	cred, err := azidentity.NewClientSecretCredential(cfg.TenantID, cfg.ClientID, cfg.ClientSecret, nil)
	if err != nil {
		return nil, fmt.Errorf("building client secret credential: %w", err)
	}
	return cred, nil
}

// oidcAssertionFunc returns a getAssertion callback for
// azidentity.NewClientAssertionCredential, sourcing the CI-issued OIDC JWT
// from whichever of cfg's OIDC fields is populated. Precedence: a literal
// token, then a token file, then a request-URL fetch (the GitHub
// Actions-style pattern). The callback re-fetches on every call so a fresh
// short-lived token is used each time Azure AD needs to mint a new access
// token, rather than reusing one captured at provider Configure time.
func oidcAssertionFunc(cfg AuthConfig) (func(context.Context) (string, error), error) {
	switch {
	case cfg.OIDCToken != "":
		token := cfg.OIDCToken
		return func(context.Context) (string, error) {
			return token, nil
		}, nil

	case cfg.OIDCTokenFilePath != "":
		path := cfg.OIDCTokenFilePath
		return func(context.Context) (string, error) {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("reading oidc_token_file_path %q: %w", path, err)
			}
			return strings.TrimSpace(string(b)), nil
		}, nil

	case cfg.OIDCRequestURL != "":
		requestURL := cfg.OIDCRequestURL
		requestToken := cfg.OIDCRequestToken
		return func(ctx context.Context) (string, error) {
			return fetchOIDCToken(ctx, requestURL, requestToken)
		}, nil
	}

	return nil, fmt.Errorf("use_oidc is enabled but none of oidc_token, oidc_token_file_path, or oidc_request_url/oidc_request_token were configured")
}

// withExchangeAudience ensures requestURL carries the audience query
// parameter Azure AD's token-exchange endpoint requires, adding it if the
// caller (or CI system) didn't already include one. Preserves any other
// existing query parameters.
func withExchangeAudience(requestURL string) (string, error) {
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("parsing OIDC request URL: %w", err)
	}
	q := u.Query()
	if q.Get("audience") == "" {
		q.Set("audience", azureADTokenExchangeAudience)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// oidcTokenResponse is the response shape returned by CI OIDC token
// endpoints (GitHub Actions' ACTIONS_ID_TOKEN_REQUEST_URL and compatible
// implementations).
type oidcTokenResponse struct {
	Value string `json:"value"`
}

// parseOIDCTokenResponse extracts the JWT from a CI OIDC token endpoint's
// JSON response body.
func parseOIDCTokenResponse(body []byte) (string, error) {
	var resp oidcTokenResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decoding OIDC token response: %w", err)
	}
	if resp.Value == "" {
		return "", fmt.Errorf("OIDC token response did not contain a token value")
	}
	return resp.Value, nil
}

var oidcHTTPClient = &http.Client{Timeout: 30 * time.Second}

// fetchOIDCToken retrieves a fresh OIDC JWT from a CI system's token
// request endpoint (the pattern GitHub Actions uses via
// ACTIONS_ID_TOKEN_REQUEST_URL / ACTIONS_ID_TOKEN_REQUEST_TOKEN, and other
// CI systems mirror). requestToken authenticates the call to the CI
// system's own token endpoint; it is unrelated to, and not the same as,
// the OIDC token being requested.
func fetchOIDCToken(ctx context.Context, requestURL, requestToken string) (string, error) {
	fullURL, err := withExchangeAudience(requestURL)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return "", fmt.Errorf("building OIDC token request: %w", err)
	}
	if requestToken != "" {
		req.Header.Set("Authorization", "Bearer "+requestToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := oidcHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting OIDC token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading OIDC token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC token endpoint returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return parseOIDCTokenResponse(body)
}
