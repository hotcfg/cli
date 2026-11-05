package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// testEnv runs the CLI against a stub API that records each request and
// answers 500. The tests in this package cover what the CLI does without the
// API: flag parsing, configuration, local validation, help and output
// formatting. End-to-end tests against the real API run with the server.
type testEnv struct {
	t    *testing.T
	ts   *httptest.Server
	home string
	env  map[string]string

	mu    sync.Mutex
	auths []string // Authorization header of each request
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{t: t, home: t.TempDir(), env: map[string]string{}}
	e.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.auths = append(e.auths, r.Header.Get("Authorization"))
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":{"code":"stub","message":"stub API"}}`)
	}))
	t.Cleanup(e.ts.Close)
	e.env["HOTCFG_API_URL"] = e.ts.URL
	return e
}

type result struct {
	code           int
	stdout, stderr string
}

// run executes the CLI. opts may adjust the App (TTY flags, stdin, ...).
func (e *testEnv) run(args []string, opts ...func(*App)) result {
	e.t.Helper()
	var stdout, stderr strings.Builder
	a := &App{
		Version:    "test",
		Stdin:      strings.NewReader(""),
		Stdout:     &stdout,
		Stderr:     &stderr,
		Getenv:     func(k string) string { return e.env[k] },
		HomeDir:    func() (string, error) { return e.home, nil },
		HTTPClient: e.ts.Client(),
	}
	for _, o := range opts {
		o(a)
	}
	code := a.Run(context.Background(), args)
	return result{code, stdout.String(), stderr.String()}
}

func (e *testEnv) mustRun(args ...string) result {
	e.t.Helper()
	r := e.run(args)
	if r.code != 0 {
		e.t.Fatalf("hotcfg %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

func (e *testEnv) writeFile(name, content string) string {
	e.t.Helper()
	p := filepath.Join(e.t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func contains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s does not contain %q:\n%s", what, w, got)
		}
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		args    []string
		wantPos []string
		wantBin string
	}{
		{[]string{"config.json", "--bin", "x"}, []string{"config.json"}, "x"},
		{[]string{"--bin", "x", "config.json"}, []string{"config.json"}, "x"},
		{[]string{"--bin=x", "a", "b"}, []string{"a", "b"}, "x"},
		{[]string{"-bin", "x", "--", "--weird.json"}, []string{"--weird.json"}, "x"},
		{[]string{"a", "--", "--bin", "y"}, []string{"a", "--bin", "y"}, ""},
		{nil, nil, ""},
	}
	for _, c := range cases {
		a := &App{Stdout: io.Discard}
		fs, _ := a.newFlagSet("push")
		bin := fs.String("bin", "", "")
		pos, err := parseArgs(fs, c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if strings.Join(pos, "|") != strings.Join(c.wantPos, "|") || *bin != c.wantBin {
			t.Errorf("%v: positional %q bin %q, want %q %q", c.args, pos, *bin, c.wantPos, c.wantBin)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	e := newTestEnv(t)
	e.env["HOTCFG_API_KEY"] = "sk_live_test"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"nope"}, `unknown command "nope"`},
		{[]string{"push", "--bin", "x"}, "missing <file.json>"},
		{[]string{"push", "f.json"}, "--bin is required"},
		{[]string{"push", "f.json", "g.json", "--bin", "x"}, `unexpected argument "g.json"`},
		{[]string{"push", "f.json", "--bin", "x", "--env", "qa"}, "--env must be production"},
		{[]string{"get", "x", "--bogus"}, "flag provided but not defined: -bogus"},
		{[]string{"get", "x", "--v", "0"}, "--v must be a version number"},
		{[]string{"diff", "x"}, "--from is required"},
		{[]string{"rollback", "x"}, "--to is required"},
		{[]string{"rollback", "x", "--to", "latest"}, "--to must be a version number"},
		{[]string{"promote", "x", "--from", "staging"}, "--from and --to are required"},
		{[]string{"promote", "x", "--from", "prod", "--to", "production"}, "must be different"},
	}
	for _, c := range cases {
		r := e.run(c.args)
		if r.code != ExitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %s)", c.args, r.code, ExitUsage, r.stderr)
		}
		contains(t, strings.Join(c.args, " "), r.stderr, c.want)
	}
	if n := len(e.auths); n != 0 {
		t.Errorf("usage errors made %d API requests", n)
	}

	r := e.run([]string{"push", "--help"})
	if r.code != 0 {
		t.Errorf("--help exit %d", r.code)
	}
	contains(t, "push --help", r.stdout, "Usage: hotcfg push <file.json> [flags]", "--bin string", "--env string", `(default "production")`, "--api-url")
	r = e.run([]string{"help"})
	contains(t, "help", r.stdout, "push", "promote", "HOTCFG_API_KEY")
	if r := e.run(nil); r.code != ExitUsage {
		t.Errorf("no args: exit %d", r.code)
	}
	if r := e.run([]string{"version"}); r.stdout != "hotcfg test\n" {
		t.Errorf("version = %q", r.stdout)
	}
}

func TestResolvePrecedence(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{}
	a := &App{
		Stderr:  io.Discard,
		Getenv:  func(k string) string { return env[k] },
		HomeDir: func() (string, error) { return home, nil },
	}
	mustResolve := func(c commonFlags) target {
		t.Helper()
		tg, err := a.resolve(&c)
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}

	// Nothing configured.
	if tg := mustResolve(commonFlags{}); tg.Key != "" || tg.APIURL != DefaultAPIURL {
		t.Errorf("empty = %+v", tg)
	}
	if err := (target{}).requireKey(); err == nil || !strings.Contains(err.Error(), "hotcfg login") {
		t.Errorf("requireKey = %v", err)
	}

	// Credentials file only.
	path, err := a.saveCredentials(Credentials{APIURL: "http://file.example:1/v1/", APIKey: "sk_live_file"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "hotcfg", "credentials"); path != want {
		t.Errorf("credentials path = %s, want %s", path, want)
	}
	tg := mustResolve(commonFlags{})
	if tg.Key != "sk_live_file" || tg.KeySource != path || tg.APIURL != "http://file.example:1" {
		t.Errorf("file = %+v", tg)
	}

	// The environment beats the file.
	env["HOTCFG_API_KEY"] = "sk_live_env"
	env["HOTCFG_API_URL"] = "http://env.example"
	tg = mustResolve(commonFlags{})
	if tg.Key != "sk_live_env" || tg.KeySource != "HOTCFG_API_KEY" || tg.APIURL != "http://env.example" {
		t.Errorf("env = %+v", tg)
	}

	// Flags beat everything.
	tg = mustResolve(commonFlags{key: "sk_live_flag", apiURL: "http://localhost:8080/"})
	if tg.Key != "sk_live_flag" || tg.KeySource != "--key" || tg.APIURL != "http://localhost:8080" {
		t.Errorf("flags = %+v", tg)
	}

	// Mixed: key from the env, URL from the file.
	delete(env, "HOTCFG_API_URL")
	tg = mustResolve(commonFlags{})
	if tg.Key != "sk_live_env" || tg.APIURL != "http://file.example:1" {
		t.Errorf("mixed = %+v", tg)
	}

	// XDG_CONFIG_HOME moves the file.
	xdg := t.TempDir()
	env["XDG_CONFIG_HOME"] = xdg
	if p, _ := a.credentialsPath(); p != filepath.Join(xdg, "hotcfg", "credentials") {
		t.Errorf("XDG path = %s", p)
	}
	delete(env, "XDG_CONFIG_HOME")

	for _, bad := range []string{"ftp://x", "localhost:8080", "https://", "https://u:p@x", "https://x/?a=b"} {
		if _, err := a.resolve(&commonFlags{apiURL: bad}); err == nil {
			t.Errorf("api url %q accepted", bad)
		}
	}

	// A corrupt file is an error that says how to fix it.
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	delete(env, "HOTCFG_API_KEY")
	if _, err := a.resolve(&commonFlags{}); err == nil || !strings.Contains(err.Error(), "hotcfg login") {
		t.Errorf("corrupt file: %v", err)
	}
}

func TestCredentialsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	home := t.TempDir()
	var stderr strings.Builder
	a := &App{Stderr: &stderr, Getenv: func(string) string { return "" }, HomeDir: func() (string, error) { return home, nil }}
	path, err := a.saveCredentials(Credentials{APIURL: DefaultAPIURL, APIKey: "sk_live_x"})
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode = %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode = %v, want 0700", di.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.loadCredentials(); err != nil {
		t.Fatal(err)
	}
	contains(t, "stderr", stderr.String(), "readable by other users", "chmod 600")
}

func TestPushInvalidLocalJSON(t *testing.T) {
	e := newTestEnv(t)
	e.env["HOTCFG_API_KEY"] = "sk_live_test"
	f := e.writeFile("broken.json", "{\n  \"a\": 1,\n  \"b\": tru\n}\n")
	r := e.run([]string{"push", f, "--bin", "x"})
	if r.code != ExitError {
		t.Errorf("exit %d", r.code)
	}
	contains(t, "stderr", r.stderr, f+":3:", "invalid JSON")
	if len(e.auths) != 0 {
		t.Error("invalid file was sent to the API")
	}
	r = e.run([]string{"push", e.writeFile("empty.json", "\n"), "--bin", "x"})
	contains(t, "empty", r.stderr, "is empty")
	r = e.run([]string{"push", "/does/not/exist.json", "--bin", "x"})
	contains(t, "missing file", r.stderr, "read /does/not/exist.json")
}

func TestUnifiedDiff(t *testing.T) {
	lines := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(s, "\n")
	}
	cases := []struct {
		name, a, b, want string
	}{
		{"equal", "a\nb", "a\nb", ""},
		{"from empty", "", "x\ny", "@@ -0,0 +1,2 @@\n+x\n+y\n"},
		{"to empty", "x", "", "@@ -1 +0,0 @@\n-x\n"},
		{"middle", "1\n2\n3\n4\n5\n6\n7\n8\n9", "1\n2\n3\n4\nFIVE\n6\n7\n8\n9",
			"@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+FIVE\n 6\n 7\n 8\n"},
		{"two hunks", "a\n1\n2\n3\n4\n5\n6\n7\n8\nb", "A\n1\n2\n3\n4\n5\n6\n7\n8\nB",
			"@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n 3\n@@ -7,4 +7,4 @@\n 6\n 7\n 8\n-b\n+B\n"},
		{"merged hunks", "a\n1\n2\n3\n4\n5\n6\nb", "A\n1\n2\n3\n4\n5\n6\nB",
			"@@ -1,8 +1,8 @@\n-a\n+A\n 1\n 2\n 3\n 4\n 5\n 6\n-b\n+B\n"},
	}
	for _, c := range cases {
		got := unifiedDiff("a", "b", lines(c.a), lines(c.b), 3, false)
		if c.want != "" {
			c.want = "--- a\n+++ b\n" + c.want
		}
		if got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}

// TestDiffLinesProperty checks on pseudo-random inputs that the edit script
// rebuilds both sides and is minimal (equal lines = longest common
// subsequence).
func TestDiffLinesProperty(t *testing.T) {
	seed := uint32(1)
	rnd := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>16) % n
	}
	gen := func() []string {
		s := make([]string, rnd(12))
		for i := range s {
			s[i] = string(rune('a' + rnd(4)))
		}
		return s
	}
	lcs := func(a, b []string) int {
		dp := make([][]int, len(a)+1)
		for i := range dp {
			dp[i] = make([]int, len(b)+1)
		}
		for i := len(a) - 1; i >= 0; i-- {
			for j := len(b) - 1; j >= 0; j-- {
				if a[i] == b[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else {
					dp[i][j] = max(dp[i+1][j], dp[i][j+1])
				}
			}
		}
		return dp[0][0]
	}
	for i := 0; i < 2000; i++ {
		a, b := gen(), gen()
		var gotA, gotB []string
		eq := 0
		for _, e := range diffLines(a, b) {
			if e.kind != editInsert {
				gotA = append(gotA, e.line)
			}
			if e.kind != editDelete {
				gotB = append(gotB, e.line)
			}
			if e.kind == editEqual {
				eq++
			}
		}
		if strings.Join(gotA, "") != strings.Join(a, "") || strings.Join(gotB, "") != strings.Join(b, "") {
			t.Fatalf("%q -> %q: script rebuilds %q -> %q", a, b, gotA, gotB)
		}
		if want := lcs(a, b); eq != want {
			t.Fatalf("%q -> %q: %d equal lines, LCS is %d", a, b, eq, want)
		}
	}
}

// A promote can fail schema validation only if the bin's schema changed
// after the source version was written, which the API cannot do yet, so the
// formatter is checked directly with a promote-shaped error.
func TestPromoteSchemaErrorFormat(t *testing.T) {
	var stderr strings.Builder
	a := &App{Stderr: &stderr}
	a.printSchemaErrors("staging payload", "ps", nil, &apiError{
		Status: 422, Code: "schema_validation_failed",
		Details: json.RawMessage(`[{"instance_location":"/n","keyword_location":"/properties/n/type","message":"expected string"}]`),
	})
	contains(t, "promote schema errors", stderr.String(),
		"staging payload does not match the schema of bin ps (1 error)", "/n", "rule:    /properties/n/type", "message: expected string")
}

func TestPointerLines(t *testing.T) {
	doc := []byte(`{
  "a": 1,
  "b": [
    10,
    {"c/d": true, "e~f": null}
  ],
  "": "empty key"
}`)
	got := pointerLines(doc)
	want := map[string]int{
		"": 1, "/a": 2, "/b": 3, "/b/0": 4, "/b/1": 5, "/b/1/c~1d": 5, "/b/1/e~0f": 5, "/": 7,
	}
	for p, l := range want {
		if got[p] != l {
			t.Errorf("line of %q = %d, want %d", p, got[p], l)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d pointers, want %d: %v", len(got), len(want), got)
	}
	if n := len(pointerLines([]byte(`{"a": [1, `))); n != 3 {
		t.Errorf("truncated document: %d pointers, want the 3 before the error", n)
	}
	if line, col := lineCol([]byte("ab\ncd"), 4); line != 2 || col != 2 {
		t.Errorf("lineCol = %d:%d", line, col)
	}
}

func TestBinPath(t *testing.T) {
	for in, want := range map[string]string{
		"flags":      "/bins/flags",
		"acme/flags": "/acme/flags",
		"a b/c?d":    "/a%20b/c%3Fd",
	} {
		if got := binPath(in, ""); got != want {
			t.Errorf("binPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := binPath("acme/flags", "/rollback"); got != "/acme/flags/rollback" {
		t.Errorf("with suffix: %q", got)
	}
}

func TestSubcommandHelp(t *testing.T) {
	e := newTestEnv(t)
	r := e.run([]string{"bins"})
	if r.code != ExitUsage {
		t.Errorf("bare group: exit %d", r.code)
	}
	contains(t, "bins", r.stderr, "bins list", "bins create")
	r = e.mustRun("help", "bins", "create")
	contains(t, "help bins create", r.stdout, "Usage: hotcfg bins create <bin_id> [flags]", "--schema", "--public")
	r = e.mustRun("env", "--help")
	contains(t, "env --help", r.stdout, "env delete")
	r = e.run([]string{"bins", "nope"})
	contains(t, "unknown subcommand", r.stderr, `unknown command "bins nope"`)
	r = e.mustRun("help")
	contains(t, "help", r.stdout, "bins list", "env delete", "history", "unpublish", "usage")
	r = e.run([]string{"bins", "create", "acme/x"})
	contains(t, "slug in create", r.stderr, "give the id alone")
	r = e.run([]string{"env", "delete", "x"})
	contains(t, "env delete without --env", r.stderr, "--env is required")
	r = e.run([]string{"history", "x", "--limit", "0"})
	contains(t, "bad limit", r.stderr, "--limit must be between 1 and 200")
	if n := len(e.auths); n != 0 {
		t.Errorf("help and usage errors made %d API requests", n)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KiB", 1536: "1.5 KiB", 1 << 20: "1 MiB", 100 << 30: "100 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
