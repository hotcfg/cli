package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---- login ----

func runLogin(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("login")
	if _, err := exactArgs(fs, args); err != nil {
		return err
	}
	key := strings.TrimSpace(common.key)
	if key == "" {
		var err error
		if key, err = a.promptKey(); err != nil {
			return err
		}
	}
	if key == "" {
		return errors.New("no API key entered")
	}
	if isPublishableKey(key) {
		return errors.New("hotcfg login needs a secret key (sk_live_...); publishable keys (pk_) are read-only")
	}
	if !isSecretKey(key) {
		return errors.New("that does not look like a hotcfg secret key: it should start with sk_live_ or sk_test_")
	}
	// The key is always the one just given; only the API URL is resolved.
	t, err := a.resolve(&commonFlags{key: key, apiURL: common.apiURL})
	if err != nil {
		return err
	}

	var me struct {
		APIKey struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Role   string `json:"role"`
			Prefix string `json:"prefix"`
			// Limits; null means every bin, every environment, never.
			Bins         []string   `json:"bins"`
			Environments []string   `json:"environments"`
			ExpiresAt    *time.Time `json:"expires_at"`
		} `json:"api_key"`
		Organization struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
			Plan string `json:"plan"`
		} `json:"organization"`
	}
	if err := a.client(t).doJSON(ctx, http.MethodGet, "/auth/me", nil, &me); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "key_expired" {
			return fmt.Errorf("%s rejected the key: %s", t.APIURL, ae.Message)
		}
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			return fmt.Errorf("%s rejected the key: it is unknown or revoked", t.APIURL)
		}
		return fmt.Errorf("could not verify the key: %w", err)
	}
	if me.APIKey.ID == "" || me.Organization.ID == "" {
		return fmt.Errorf("could not verify the key: %s did not identify it (is --api-url a hotcfg API?)", t.APIURL)
	}
	if me.APIKey.Role != "admin" {
		return fmt.Errorf("key %s is %s; hotcfg login needs a secret (admin) key", redactKey(key), me.APIKey.Role)
	}

	path, err := a.saveCredentials(Credentials{APIURL: t.APIURL, APIKey: key})
	if err != nil {
		return err
	}
	p := a.printer()
	p.ok("Logged in to %s (%s)", me.Organization.Name, cmp.Or(me.Organization.Slug, me.Organization.ID))
	p.field("key", fmt.Sprintf("%s  %s", redactKey(key), me.APIKey.Name))
	p.field("api", t.APIURL)
	if k := me.APIKey; k.Bins != nil || k.Environments != nil {
		p.field("bins", cmp.Or(strings.Join(k.Bins, ", "), "all"))
		p.field("envs", cmp.Or(strings.Join(k.Environments, ", "), "all"))
	}
	if k := me.APIKey; k.ExpiresAt != nil {
		p.field("expires", k.ExpiresAt.Local().Format("2006-01-02 15:04"))
	}
	p.field("saved", path)
	if env := a.Getenv("HOTCFG_API_KEY"); env != "" && env != key {
		fmt.Fprintln(a.Stderr, "note: HOTCFG_API_KEY is set and takes precedence over the saved key")
	}
	return nil
}

// promptKey asks for the key, hiding input on a terminal. Without a
// terminal it reads one line, so "echo $KEY | hotcfg login" works.
func (a *App) promptKey() (string, error) {
	if a.StdinTTY {
		fmt.Fprint(a.Stderr, "Secret API key (sk_live_...): ")
	}
	var line string
	var err error
	if a.ReadSecret != nil {
		line, err = a.ReadSecret()
		if a.StdinTTY {
			fmt.Fprintln(a.Stderr)
		}
	} else {
		line, err = bufio.NewReader(a.Stdin).ReadString('\n')
		if errors.Is(err, io.EOF) && line != "" {
			err = nil
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return "", errors.New("no API key given: pass --key or enter it when prompted")
		}
		return "", fmt.Errorf("read API key: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// ---- push ----

func runPush(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("push")
	binID := fs.String("bin", "", "bin to write to: an id, or org-slug/id (required)")
	envFlag := fs.String("env", "production", "environment: production, staging or development")
	pos, err := exactArgs(fs, args, "<file.json>")
	if err != nil {
		return err
	}
	if *binID == "" {
		return usagef("--bin is required")
	}
	env, err := parseEnvFlag("env", *envFlag)
	if err != nil {
		return err
	}
	file := pos[0]
	var payload []byte
	if file == "-" {
		file = "<stdin>"
		payload, err = io.ReadAll(a.Stdin)
	} else {
		payload, err = os.ReadFile(file)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	if err := checkJSON(file, payload); err != nil {
		return err
	}

	t, err := a.resolve(common)
	if err != nil {
		return err
	}
	if err := t.requireKey(); err != nil {
		return err
	}
	b, _, err := a.client(t).do(ctx, http.MethodPut, binPath(*binID, ""), url.Values{"env": {env}}, payload)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "schema_validation_failed" {
			a.printSchemaErrors(file, *binID, payload, ae)
			return errReported
		}
		return err
	}
	var res writeResult
	if err := json.Unmarshal(b, &res); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	p := a.printer()
	p.ok("Pushed %s to %s (%s)", file, res.BinID, res.Environment)
	p.field("version", fmt.Sprintf("v%d", res.Version))
	p.field("hash", res.Hash)
	p.field("cache", fmt.Sprintf("edge purge queued for %s", res.Environment))
	return nil
}

// checkJSON rejects a file that is not exactly one JSON value, pointing at
// the line and column of the problem.
func checkJSON(file string, b []byte) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return fmt.Errorf("%s is empty", file)
	}
	var v json.RawMessage
	err := json.Unmarshal(b, &v)
	if err == nil {
		return nil
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		// Offset counts the bytes read, including the offending one.
		line, col := lineCol(b, int(se.Offset)-1)
		return fmt.Errorf("%s:%d:%d: invalid JSON: %s", file, line, col, se.Error())
	}
	return fmt.Errorf("%s: invalid JSON: %w", file, err)
}

// schemaError is one entry of a 422 schema_validation_failed response.
type schemaError struct {
	InstanceLocation string `json:"instance_location"`
	KeywordLocation  string `json:"keyword_location"`
	Message          string `json:"message"`
}

// printSchemaErrors reports schema violations with the file, the line of
// the offending value, the schema rule and the message.
func (a *App) printSchemaErrors(file, binID string, payload []byte, ae *apiError) {
	var errs []schemaError
	_ = json.Unmarshal(ae.Details, &errs)
	w := a.Stderr
	n := len(errs)
	plural := "s"
	if n == 1 {
		plural = ""
	}
	fmt.Fprintf(w, "error: %s does not match the schema of bin %s (%d error%s)\n", file, binID, n, plural)
	if n == 0 {
		fmt.Fprintf(w, "  %s\n", ae.Message)
		return
	}
	lines := pointerLines(payload)
	for _, e := range errs {
		loc := file
		if line, ok := lines[e.InstanceLocation]; ok {
			loc += ":" + strconv.Itoa(line)
		}
		ptr := e.InstanceLocation
		if ptr == "" {
			ptr = "(document root)"
		}
		fmt.Fprintf(w, "\n  %s  %s\n", loc, ptr)
		if e.KeywordLocation != "" {
			fmt.Fprintf(w, "    rule:    %s\n", e.KeywordLocation)
		}
		fmt.Fprintf(w, "    message: %s\n", e.Message)
	}
}

// ---- get ----

func runGet(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("get")
	envFlag := fs.String("env", "production", "environment: production, staging or development")
	vFlag := fs.String("v", "latest", "version: latest, or a number such as 3 or v3")
	query := fs.String("query", "", "jq filter applied by the API, e.g. '.features.checkout'")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	env, err := parseEnvFlag("env", *envFlag)
	if err != nil {
		return err
	}
	version, err := parseVersion("v", *vFlag, true)
	if err != nil {
		return err
	}
	t, err := a.resolve(common)
	if err != nil {
		return err
	}
	q := url.Values{"env": {env}}
	if version != "latest" {
		q.Set("v", version)
	}
	if *query != "" {
		q.Set("query", *query)
	}
	b, _, err := a.client(t).do(ctx, http.MethodGet, binPath(pos[0], ""), q, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && t.Key == "" {
		if !strings.Contains(pos[0], "/") {
			// Without a key there is no organization to look a bare id up in.
			return fmt.Errorf("%w\nhint: without an API key, name the bin with its organization: hotcfg get <org-slug>/%s", err, pos[0])
		}
		if ae.Message == "bin not found" {
			// The API answers a private bin read without a key like a
			// missing bin, so it cannot be told apart from a typo.
			return fmt.Errorf("%w\nhint: if the bin is private, it needs an API key: --key, $HOTCFG_API_KEY or 'hotcfg login'", err)
		}
	}
	if err != nil {
		return err
	}
	return a.writeJSONOut(b)
}

// writeJSONOut prints a JSON body: indented on a terminal, compact (as the
// API sent it) when piped.
func (a *App) writeJSONOut(b []byte) error {
	if a.StdoutTTY {
		var buf bytes.Buffer
		if err := json.Indent(&buf, b, "", "  "); err == nil {
			b = buf.Bytes()
		}
	}
	if _, err := a.Stdout.Write(b); err != nil {
		return err
	}
	_, err := io.WriteString(a.Stdout, "\n")
	return err
}

// parseVersion accepts "3", "v3" and (if allowLatest) "latest". It returns
// the number as a string, or "latest".
func parseVersion(flagName, v string, allowLatest bool) (string, error) {
	v = strings.TrimSpace(v)
	if allowLatest && strings.EqualFold(v, "latest") {
		return "latest", nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(v), "v"))
	if err != nil || n < 1 {
		want := "a version number such as 3 or v3"
		if allowLatest {
			want += ", or latest"
		}
		return "", usagef("--%s must be %s, not %q", flagName, want, v)
	}
	return strconv.Itoa(n), nil
}

// ---- diff ----

func runDiff(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("diff")
	envFlag := fs.String("env", "production", "environment: production, staging or development")
	from := fs.String("from", "", "old version, e.g. v3 (required)")
	to := fs.String("to", "latest", "new version, e.g. v5, or latest")
	ctxLines := fs.Int("context", 3, "lines of context around each change")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	if *from == "" {
		return usagef("--from is required")
	}
	env, err := parseEnvFlag("env", *envFlag)
	if err != nil {
		return err
	}
	vA, err := parseVersion("from", *from, true)
	if err != nil {
		return err
	}
	vB, err := parseVersion("to", *to, true)
	if err != nil {
		return err
	}
	if *ctxLines < 0 {
		return usagef("--context must not be negative")
	}
	t, err := a.resolve(common)
	if err != nil {
		return err
	}
	c := a.client(t)
	binID := pos[0]

	fetch := func(v string) ([]string, string, error) {
		q := url.Values{"env": {env}}
		if v != "latest" {
			q.Set("v", v)
		}
		b, h, err := c.do(ctx, http.MethodGet, binPath(binID, ""), q, nil)
		if err != nil {
			return nil, "", fmt.Errorf("fetch %s %s: %w", binID, versionLabel(v), err)
		}
		if got := h.Get("X-Hotcfg-Version"); got != "" {
			v = got
		}
		lines, err := canonicalLines(b)
		if err != nil {
			return nil, "", fmt.Errorf("fetch %s %s: %w", binID, versionLabel(v), err)
		}
		return lines, versionLabel(v), nil
	}
	a1, labelA, err := fetch(vA)
	if err != nil {
		return err
	}
	b1, labelB, err := fetch(vB)
	if err != nil {
		return err
	}
	out := unifiedDiff(
		fmt.Sprintf("%s %s %s", binID, env, labelA),
		fmt.Sprintf("%s %s %s", binID, env, labelB),
		a1, b1, *ctxLines, a.color())
	if out == "" {
		a.printer().ok("No differences between %s and %s (%s)", labelA, labelB, env)
		return nil
	}
	_, err = io.WriteString(a.Stdout, out)
	return err
}

func versionLabel(v string) string {
	if v == "latest" {
		return v
	}
	return "v" + v
}

// canonicalLines pretty-prints a JSON document with sorted object keys and
// exact numbers, so diffs show changes in content, not in key order.
func canonicalLines(b []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("response is not JSON: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n"), nil
}

// ---- rollback ----

func runRollback(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("rollback")
	envFlag := fs.String("env", "production", "environment: production, staging or development")
	to := fs.String("to", "", "version to restore, e.g. 3 or v3 (required)")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	if *to == "" {
		return usagef("--to is required")
	}
	env, err := parseEnvFlag("env", *envFlag)
	if err != nil {
		return err
	}
	v, err := parseVersion("to", *to, false)
	if err != nil {
		return err
	}
	version, _ := strconv.Atoi(v)
	t, err := a.resolve(common)
	if err != nil {
		return err
	}
	if err := t.requireKey(); err != nil {
		return err
	}
	var res writeResult
	req := map[string]any{"env": env, "version": version}
	if err := a.client(t).doJSON(ctx, http.MethodPost, binPath(pos[0], "/rollback"), req, &res); err != nil {
		return err
	}
	p := a.printer()
	p.ok("Rolled back %s (%s) to v%d", res.BinID, res.Environment, version)
	p.field("version", fmt.Sprintf("v%d (new revision with the contents of v%d)", res.Version, version))
	p.field("hash", res.Hash)
	p.field("cache", fmt.Sprintf("edge purge queued for %s", res.Environment))
	return nil
}

// ---- promote ----

func runPromote(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("promote")
	fromFlag := fs.String("from", "", "source environment, e.g. staging (required)")
	toFlag := fs.String("to", "", "target environment, e.g. production (required)")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	if *fromFlag == "" || *toFlag == "" {
		return usagef("--from and --to are required")
	}
	from, err := parseEnvFlag("from", *fromFlag)
	if err != nil {
		return err
	}
	to, err := parseEnvFlag("to", *toFlag)
	if err != nil {
		return err
	}
	if from == to {
		return usagef("--from and --to must be different environments")
	}
	t, err := a.resolve(common)
	if err != nil {
		return err
	}
	if err := t.requireKey(); err != nil {
		return err
	}
	var res writeResult
	req := map[string]string{"from_env": from, "to_env": to}
	if err := a.client(t).doJSON(ctx, http.MethodPost, binPath(pos[0], "/promote"), req, &res); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "schema_validation_failed" {
			a.printSchemaErrors(from+" payload", pos[0], nil, ae)
			return errReported
		}
		return err
	}
	p := a.printer()
	p.ok("Promoted %s %s v%d → %s v%d", res.BinID, from, res.PromotedFromVersion, res.Environment, res.Version)
	p.field("hash", res.Hash)
	p.field("cache", fmt.Sprintf("edge purge queued for %s", res.Environment))
	return nil
}

// ---- output ----

type printer struct {
	w     io.Writer
	color bool
}

func (a *App) printer() printer { return printer{a.Stdout, a.color()} }

func (p printer) ok(format string, args ...any) {
	mark := "✓"
	if p.color {
		mark = ansiGreen + mark + ansiReset
	}
	fmt.Fprintf(p.w, "%s %s\n", mark, fmt.Sprintf(format, args...))
}

func (p printer) field(name, value string) {
	label := fmt.Sprintf("%-8s", name)
	if p.color {
		label = ansiDim + label + ansiReset
	}
	fmt.Fprintf(p.w, "  %s %s\n", label, value)
}
