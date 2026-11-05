package cli

import (
	"bufio"
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
	"text/tabwriter"
	"time"
)

// Commands for managing bins rather than their payloads: listing and
// creating bins, history, unpublishing, deleting an environment, and the
// organization's usage. Listing commands print a table, or the API's JSON
// with --json.

// envOrder is the order environments are shown in.
var envOrder = []string{"production", "staging", "development"}

// authed resolves the target and requires a key; every command here needs one.
func (a *App) authed(common *commonFlags) (*client, error) {
	t, err := a.resolve(common)
	if err != nil {
		return nil, err
	}
	if err := t.requireKey(); err != nil {
		return nil, err
	}
	return a.client(t), nil
}

func (a *App) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
}

// localTime formats an API timestamp for a table.
func localTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// ---- bins list ----

type binSummary struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	IsPublic    bool      `json:"is_public"`
	HasSchema   bool      `json:"has_schema"`
	UpdatedAt   time.Time `json:"updated_at"`
	Envs        map[string]struct {
		Version int `json:"version"`
	} `json:"environments"`
}

func runBinsList(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("bins list")
	asJSON := fs.Bool("json", false, "print the API's JSON response")
	if _, err := exactArgs(fs, args); err != nil {
		return err
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	b, _, err := c.do(ctx, http.MethodGet, "/bins", nil, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.writeJSONOut(b)
	}
	var res struct {
		Bins []binSummary `json:"bins"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	if len(res.Bins) == 0 {
		fmt.Fprintln(a.Stdout, "No bins yet. Create one with 'hotcfg bins create <bin_id>'.")
		return nil
	}
	w := a.table()
	fmt.Fprintln(w, "ID\tNAME\tACCESS\tENVIRONMENTS\tUPDATED")
	for _, bin := range res.Bins {
		var envs []string
		for _, env := range envOrder {
			if st, ok := bin.Envs[env]; ok {
				envs = append(envs, fmt.Sprintf("%s v%d", env, st.Version))
			}
		}
		access := "private"
		if bin.IsPublic {
			access = "public"
		}
		if bin.HasSchema {
			access += ", schema"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", bin.ID, bin.Name, access,
			cmp.Or(strings.Join(envs, ", "), "-"), localTime(bin.UpdatedAt))
	}
	return w.Flush()
}

// ---- bins create ----

func runBinsCreate(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("bins create")
	name := fs.String("name", "", "display name (default: the id)")
	desc := fs.String("description", "", "description")
	schemaFile := fs.String("schema", "", "JSON Schema file every version must match")
	public := fs.Bool("public", false, "let anyone read it without a key")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	id := pos[0]
	if strings.Contains(id, "/") {
		return usagef("bins are created in your key's organization: give the id alone, not %q", id)
	}
	req := map[string]any{"id": id, "is_public": *public}
	if *name != "" {
		req["name"] = *name
	}
	if *desc != "" {
		req["description"] = *desc
	}
	if *schemaFile != "" {
		schema, err := os.ReadFile(*schemaFile)
		if err != nil {
			return fmt.Errorf("read %s: %w", *schemaFile, err)
		}
		if err := checkJSON(*schemaFile, schema); err != nil {
			return err
		}
		req["schema"] = json.RawMessage(schema)
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	var res struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		IsPublic  bool   `json:"is_public"`
		HasSchema bool   `json:"has_schema"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/bins", req, &res); err != nil {
		return err
	}
	p := a.printer()
	p.ok("Created bin %s", res.ID)
	p.field("name", res.Name)
	access := "private: reads need a key"
	if res.IsPublic {
		access = "public: anyone can read it"
	}
	p.field("access", access)
	if res.HasSchema {
		p.field("schema", *schemaFile)
	}
	p.field("next", fmt.Sprintf("hotcfg push config.json --bin %s --env staging", res.ID))
	return nil
}

// ---- history ----

func runHistory(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("history")
	envFlag := fs.String("env", "production", "environment: production, staging or development")
	limit := fs.Int("limit", 20, "how many versions to show, newest first (at most 200)")
	asJSON := fs.Bool("json", false, "print the API's JSON response")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	env, err := parseEnvFlag("env", *envFlag)
	if err != nil {
		return err
	}
	if *limit < 1 || *limit > 200 {
		return usagef("--limit must be between 1 and 200")
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	q := url.Values{"env": {env}, "limit": {strconv.Itoa(*limit)}}
	b, _, err := c.do(ctx, http.MethodGet, binPath(pos[0], "/history"), q, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.writeJSONOut(b)
	}
	var res struct {
		BinID    string `json:"bin_id"`
		Current  int    `json:"current_version"`
		Versions []struct {
			Version    int       `json:"version"`
			Hash       string    `json:"hash"`
			CreatedAt  time.Time `json:"created_at"`
			AuthorType string    `json:"author_type"`
			AuthorName *string   `json:"author_name"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	if len(res.Versions) == 0 {
		fmt.Fprintf(a.Stdout, "%s has no versions in %s.\n", res.BinID, env)
		return nil
	}
	w := a.table()
	fmt.Fprintln(w, "VERSION\tCREATED\tAUTHOR\tHASH")
	for _, v := range res.Versions {
		label := fmt.Sprintf("v%d", v.Version)
		if v.Version == res.Current {
			label += " (current)"
		}
		author := "deleted user"
		if v.AuthorName != nil {
			author = *v.AuthorName
		} else if v.AuthorType == "api_key" {
			author = "deleted key"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", label, localTime(v.CreatedAt), author, shortHash(v.Hash))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if res.Current == 0 {
		fmt.Fprintf(a.Stdout, "\n%s is unpublished in %s: nothing is served until the next push or rollback.\n", res.BinID, env)
	}
	if len(res.Versions) == *limit {
		fmt.Fprintf(a.Stdout, "\nShowing the newest %d; pass --limit for more.\n", *limit)
	}
	return nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// ---- unpublish ----

func runUnpublish(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("unpublish")
	envFlag := fs.String("env", "", "environment to take down: production, staging or development (required)")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	// No default: taking production down must be asked for by name.
	if *envFlag == "" {
		return usagef("--env is required")
	}
	env, err := parseEnvFlag("env", *envFlag)
	if err != nil {
		return err
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	var res struct {
		BinID       string `json:"bin_id"`
		Environment string `json:"environment"`
		Unpublished int    `json:"unpublished_version"`
	}
	if err := c.doJSON(ctx, http.MethodPost, binPath(pos[0], "/unpublish"), map[string]string{"env": env}, &res); err != nil {
		return err
	}
	p := a.printer()
	p.ok("Unpublished %s (%s): v%d is no longer served", res.BinID, res.Environment, res.Unpublished)
	p.field("history", fmt.Sprintf("kept; publish again with 'hotcfg rollback %s --env %s --to %d'", pos[0], res.Environment, res.Unpublished))
	p.field("cache", fmt.Sprintf("edge purge queued for %s", res.Environment))
	return nil
}

// ---- env delete ----

func runEnvDelete(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("env delete")
	envFlag := fs.String("env", "", "environment to delete: production, staging or development (required)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	if *envFlag == "" {
		return usagef("--env is required")
	}
	env, err := parseEnvFlag("env", *envFlag)
	if err != nil {
		return err
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	binID := pos[0]
	if !*yes {
		if !a.StdinTTY {
			return fmt.Errorf("deleting %s %s erases its whole history; pass --yes to confirm", binID, env)
		}
		fmt.Fprintf(a.Stderr, "This deletes every version of %s in %s; the next push starts again at v1.\nType the bin id to confirm: ", binID, env)
		line, err := bufio.NewReader(a.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if strings.TrimSpace(line) != binID {
			return errors.New("not confirmed; nothing was deleted")
		}
	}
	if _, _, err := c.do(ctx, http.MethodDelete, binPath(binID, "/environments/"+env), nil, nil); err != nil {
		return err
	}
	p := a.printer()
	p.ok("Deleted %s (%s) and its history", binID, env)
	p.field("cache", fmt.Sprintf("edge purge queued for %s", env))
	return nil
}

// ---- usage ----

func runUsage(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("usage")
	asJSON := fs.Bool("json", false, "print the API's JSON response")
	if _, err := exactArgs(fs, args); err != nil {
		return err
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	var me struct {
		Organization struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"organization"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/auth/me", nil, &me); err != nil {
		return err
	}
	if me.Organization.ID == "" {
		return errors.New("this key does not belong to an organization")
	}
	b, _, err := c.do(ctx, http.MethodGet, "/organizations/"+url.PathEscape(me.Organization.ID)+"/usage", nil, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.writeJSONOut(b)
	}
	var res struct {
		Plan  string `json:"plan"`
		Usage struct {
			Bins    int64 `json:"bins"`
			Seats   int64 `json:"seats"`
			Storage int64 `json:"storage_bytes"`
		} `json:"usage"`
		Limits struct {
			MaxBins          int64 `json:"max_bins"`
			MaxSeats         int64 `json:"max_seats"`
			MaxPayloadBytes  int64 `json:"max_payload_bytes"`
			RetainedVersions int64 `json:"retained_versions"`
			MaxStorageBytes  int64 `json:"max_storage_bytes"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	l := res.Limits
	fmt.Fprintf(a.Stdout, "%s (%s), %s plan\n\n", me.Organization.Name, cmp.Or(me.Organization.Slug, me.Organization.ID), res.Plan)
	w := a.table()
	fmt.Fprintf(w, "bins\t%d of %s\n", res.Usage.Bins, limitText(l.MaxBins, strconv.FormatInt))
	fmt.Fprintf(w, "seats\t%d of %s\n", res.Usage.Seats, limitText(l.MaxSeats, strconv.FormatInt))
	fmt.Fprintf(w, "storage\t%s of %s\n", humanBytes(res.Usage.Storage), limitText(l.MaxStorageBytes, bytesText))
	fmt.Fprintf(w, "payload\tup to %s per version\n", limitText(l.MaxPayloadBytes, bytesText))
	fmt.Fprintf(w, "history\t%s versions kept per environment\n", limitText(l.RetainedVersions, strconv.FormatInt))
	return w.Flush()
}

// limitText formats a limit, where -1 means unlimited.
func limitText(n int64, f func(int64, int) string) string {
	if n < 0 {
		return "unlimited"
	}
	return f(n, 10)
}

func bytesText(n int64, _ int) string { return humanBytes(n) }

// humanBytes formats a size in binary units: 512 B, 1.5 KiB, 2 MiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n) / unit
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + " " + units[i]
}

// groups are commands with subcommands ("bins list"): their names, for
// Run's dispatch and the group's own help.
func groupCommands(group string) []command {
	var out []command
	for _, c := range commands {
		if g, _, ok := strings.Cut(c.name, " "); ok && g == group {
			out = append(out, c)
		}
	}
	return out
}

func isGroup(name string) bool { return len(groupCommands(name)) > 0 }

func (a *App) groupUsage(w io.Writer, group string) {
	fmt.Fprintf(w, "Usage: hotcfg %s <command> [arguments] [flags]\n\nCommands:\n", group)
	for _, c := range groupCommands(group) {
		fmt.Fprintf(w, "  %-12s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "\nRun 'hotcfg %s <command> --help' for a command's flags.\n", group)
}
