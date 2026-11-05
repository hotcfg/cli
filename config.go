package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Credentials is the saved login, ~/.config/hotcfg/credentials.
type Credentials struct {
	APIURL string `json:"api_url"`
	APIKey string `json:"api_key"`
}

// credentialsPath is $XDG_CONFIG_HOME/hotcfg/credentials, or
// ~/.config/hotcfg/credentials (on every OS, so the path is predictable).
func (a *App) credentialsPath() (string, error) {
	if x := a.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "hotcfg", "credentials"), nil
	}
	home, err := a.HomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".config", "hotcfg", "credentials"), nil
}

// loadCredentials returns the saved credentials, or nil if there are none.
func (a *App) loadCredentials() (*Credentials, error) {
	path, err := a.credentialsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(a.Stderr, "warning: %s is readable by other users; run: chmod 600 %s\n", path, path)
		}
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("credentials file %s is not valid JSON: %w (run 'hotcfg login' to rewrite it)", path, err)
	}
	return &c, nil
}

// saveCredentials writes the credentials file with mode 0600, atomically,
// and returns its path.
func (a *App) saveCredentials(c Credentials) (string, error) {
	path, err := a.credentialsPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*") // created 0600
	if err != nil {
		return "", fmt.Errorf("write credentials: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return "", fmt.Errorf("write credentials: %w", err)
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("write credentials: %w", err)
	}
	return path, nil
}

// target is where a command sends requests and with which key.
type target struct {
	APIURL    string
	Key       string
	KeySource string // "--key", "HOTCFG_API_KEY", the credentials path, or ""
}

// resolve applies the precedence rules:
//
//	key:     --key, then $HOTCFG_API_KEY, then the credentials file
//	API URL: --api-url, then $HOTCFG_API_URL, then the credentials file,
//	         then DefaultAPIURL
//
// The credentials file is only read when something is still missing.
func (a *App) resolve(c *commonFlags) (target, error) {
	t := target{APIURL: c.apiURL, Key: strings.TrimSpace(c.key)}
	if t.Key != "" {
		t.KeySource = "--key"
	} else if k := strings.TrimSpace(a.Getenv("HOTCFG_API_KEY")); k != "" {
		t.Key, t.KeySource = k, "HOTCFG_API_KEY"
	}
	if t.APIURL == "" {
		t.APIURL = a.Getenv("HOTCFG_API_URL")
	}
	if t.Key == "" || t.APIURL == "" {
		creds, err := a.loadCredentials()
		if err != nil {
			return target{}, err
		}
		if creds != nil {
			if t.Key == "" && creds.APIKey != "" {
				t.Key = creds.APIKey
				t.KeySource, _ = a.credentialsPath()
			}
			if t.APIURL == "" {
				t.APIURL = creds.APIURL
			}
		}
	}
	if t.APIURL == "" {
		t.APIURL = DefaultAPIURL
	}
	u, err := normalizeAPIURL(t.APIURL)
	if err != nil {
		return target{}, err
	}
	t.APIURL = u
	return t, nil
}

// normalizeAPIURL checks an API base URL and strips a trailing slash or
// "/v1", so both "http://localhost:8080" and ".../v1/" work.
func normalizeAPIURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid API URL %q: want e.g. https://api.hotcfg.run or http://localhost:8080", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("invalid API URL %q: it must not contain credentials, a query or a fragment", raw)
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimSuffix(p, "/v1")
	u.Path, u.RawPath = p, ""
	return u.String(), nil
}

// requireKey fails with a hint when no key is configured.
func (t target) requireKey() error {
	if t.Key == "" {
		return errors.New("no API key: run 'hotcfg login', set HOTCFG_API_KEY, or pass --key")
	}
	return nil
}

func isSecretKey(k string) bool {
	return strings.HasPrefix(k, "sk_live_") || strings.HasPrefix(k, "sk_test_")
}

func isPublishableKey(k string) bool {
	return strings.HasPrefix(k, "pk_live_") || strings.HasPrefix(k, "pk_test_")
}

// redactKey shows enough of a key to recognize it.
func redactKey(k string) string {
	const show = 12 // "sk_live_" + 4
	if len(k) <= show {
		return strings.Repeat("•", len(k))
	}
	return k[:show] + "…"
}

// envAliases maps accepted --env spellings to API environment names.
var envAliases = map[string]string{
	"production": "production", "prod": "production",
	"staging": "staging", "stage": "staging", "stg": "staging",
	"development": "development", "dev": "development",
}

func parseEnvFlag(flagName, v string) (string, error) {
	if env, ok := envAliases[strings.ToLower(strings.TrimSpace(v))]; ok {
		return env, nil
	}
	return "", usagef("--%s must be production (prod), staging (stage) or development (dev), not %q", flagName, v)
}
