package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// exportTimeout bounds one export download. An organization's export can be
// far larger than any other response, so it gets more than the client's
// usual request timeout.
const exportTimeout = 10 * time.Minute

// runExport downloads GET /organizations/{id}/export: every bin with its
// settings, schema and kept versions, as one JSON file. The body is
// streamed to a temporary file and only renamed into place once it is
// complete, valid JSON, so a cut-off download never looks like an export.
func runExport(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("export")
	out := fs.String("o", "", "output file (default: hotcfg-<organization>-<date>.json; - for stdout)")
	fs.StringVar(out, "out", "", "same as -o")
	current := fs.Bool("current", false, "only each environment's current version, not the whole history")
	if _, err := exactArgs(fs, args); err != nil {
		return err
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	org, err := keyOrganization(ctx, c)
	if err != nil {
		return err
	}
	var q url.Values
	if *current {
		q = url.Values{"versions": {"current"}}
	}
	if c.http.Timeout > 0 && c.http.Timeout < exportTimeout {
		hc := *c.http
		hc.Timeout = exportTimeout
		c.http = &hc
	}
	body, err := c.stream(ctx, "/organizations/"+url.PathEscape(org.ID)+"/export", q)
	if err != nil {
		return err
	}
	defer body.Close()

	if *out == "-" {
		return copyValidJSON(a.Stdout, body)
	}
	path := *out
	if path == "" {
		slug := org.Slug
		if slug == "" {
			slug = org.ID
		}
		path = fmt.Sprintf("hotcfg-%s-%s.json", slug, time.Now().Format("2006-01-02"))
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hotcfg-export-*.json")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	err = copyValidJSON(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	info, _ := os.Stat(path)
	p := a.printer()
	p.ok("Exported %s to %s", org.Name, path)
	if info != nil {
		p.field("size", humanBytes(info.Size()))
	}
	if *current {
		p.field("versions", "current only")
	} else {
		p.field("versions", "all kept versions")
	}
	return nil
}

// copyValidJSON copies one JSON document from r to w and fails if it is
// not complete, valid JSON: the server cuts an export short when it fails
// partway, after the 200 status was sent.
func copyValidJSON(w io.Writer, r io.Reader) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := checkExport(json.NewDecoder(pr))
		io.Copy(io.Discard, pr) // drain, so the writer never blocks
		done <- err
	}()
	_, err := io.Copy(io.MultiWriter(w, pw), r)
	pw.CloseWithError(err)
	if err != nil {
		<-done
		return fmt.Errorf("download export: %w", err)
	}
	if err := <-done; err != nil {
		return fmt.Errorf("the export is incomplete or invalid (%v); try again", err)
	}
	return nil
}

// checkExport reads one JSON document token by token, so the export never
// sits in memory. The server writes "format" as the first key.
func checkExport(dec *json.Decoder) error {
	for i, want := range []any{json.Delim('{'), "format"} {
		tok, err := dec.Token()
		if err != nil || tok != want {
			if i == 0 && err != nil {
				return err
			}
			return errors.New("not a hotcfg export")
		}
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the export")
	}
	return nil
}
