package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// schedule is a scheduled promote, as the API returns it.
type schedule struct {
	ID              string     `json:"id"`
	BinID           string     `json:"bin_id"`
	FromEnv         string     `json:"from_env"`
	FromVersion     int        `json:"from_version"`
	ToEnv           string     `json:"to_env"`
	At              time.Time  `json:"at"`
	RevertAt        *time.Time `json:"revert_at"`
	State           string     `json:"state"`
	Version         int        `json:"version"`
	PreviousVersion int        `json:"previous_version"`
	RevertedVersion int        `json:"reverted_version"`
	Error           string     `json:"error"`
	Actor           struct {
		Type string  `json:"type"`
		Name *string `json:"name"`
	} `json:"actor"`
}

// scheduleTimeFlag reads --at or --revert-at: RFC 3339, or "now".
func scheduleTimeFlag(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return "", nil
	case strings.EqualFold(v, "now"):
		return time.Now().UTC().Format(time.RFC3339), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", usagef("--%s must be a time with its zone, such as 2026-11-21T18:00:00Z or 2026-11-21T19:00:00+01:00, or now", name)
	}
	return t.UTC().Format(time.RFC3339), nil
}

func runSchedulesList(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("schedules list")
	asJSON := fs.Bool("json", false, "print the API's JSON response")
	pos, err := exactArgs(fs, args, "<bin_id>")
	if err != nil {
		return err
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	b, _, err := c.do(ctx, http.MethodGet, binPath(pos[0], "/schedules"), nil, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return a.writeJSONOut(b)
	}
	var res struct {
		Schedules []schedule `json:"schedules"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	if len(res.Schedules) == 0 {
		fmt.Fprintf(a.Stdout, "%s has no scheduled promotes.\n", pos[0])
		return nil
	}
	w := a.table()
	fmt.Fprintln(w, "ID\tPROMOTE\tAT\tREVERT AT\tSTATE\tBY")
	for _, s := range res.Schedules {
		revert := "-"
		if s.RevertAt != nil {
			revert = localTime(*s.RevertAt)
		}
		by := "deleted"
		if s.Actor.Name != nil {
			by = oneLine(*s.Actor.Name)
		}
		fmt.Fprintf(w, "%s\t%s v%d → %s\t%s\t%s\t%s\t%s\n", s.ID, s.FromEnv, s.FromVersion, s.ToEnv, localTime(s.At), revert, scheduleState(s), by)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, s := range res.Schedules {
		if s.Error != "" {
			fmt.Fprintf(a.Stdout, "\n%s: %s\n", oneLine(s.ID), oneLine(s.Error))
		}
	}
	return nil
}

// scheduleState says what happened, in a few words.
func scheduleState(s schedule) string {
	switch s.State {
	case "pending":
		return "waiting"
	case "waiting_revert":
		return fmt.Sprintf("promoted as v%d, revert waiting", s.Version)
	case "done":
		return fmt.Sprintf("promoted as v%d", s.Version)
	case "reverted":
		return fmt.Sprintf("promoted as v%d, reverted as v%d", s.Version, s.RevertedVersion)
	case "revert_skipped":
		return fmt.Sprintf("promoted as v%d, revert skipped", s.Version)
	}
	return strings.ReplaceAll(s.State, "_", " ")
}

func runSchedulesCancel(ctx context.Context, a *App, args []string) error {
	fs, common := a.newFlagSet("schedules cancel")
	pos, err := exactArgs(fs, args, "<bin_id>", "<schedule_id>")
	if err != nil {
		return err
	}
	c, err := a.authed(common)
	if err != nil {
		return err
	}
	if _, _, err := c.do(ctx, http.MethodDelete, binPath(pos[0], "/schedules/"+url.PathEscape(pos[1])), nil, nil); err != nil {
		return err
	}
	a.printer().ok("Cancelled %s on %s", pos[1], pos[0])
	return nil
}
