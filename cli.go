// Package cli implements the hotcfg command-line tool.
//
// It uses only the standard library: commands are flag.FlagSets, and flags
// may appear before or after positional arguments
// ("hotcfg push config.json --bin app-flags").
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// DefaultAPIURL is used when neither --api-url, HOTCFG_API_URL nor the
// credentials file names an API.
const DefaultAPIURL = "https://api.hotcfg.run"

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1 // the command ran and failed (API error, invalid file, ...)
	ExitUsage = 2 // bad command line
)

// App is one CLI invocation's environment. Tests build it directly; main
// uses NewApp.
type App struct {
	Version string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// StdoutTTY selects human output: pretty JSON and colors.
	StdoutTTY bool
	// StdinTTY means a person can be prompted.
	StdinTTY bool

	Getenv  func(string) string
	HomeDir func() (string, error)

	HTTPClient *http.Client

	// ReadSecret reads a line without echoing it. Nil: read a plain line
	// from Stdin (used when Stdin is not a terminal, and in tests).
	ReadSecret func() (string, error)
}

// NewApp returns an App wired to the process's terminal and environment.
func NewApp(version string) *App {
	a := &App{
		Version:    version,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		StdoutTTY:  isTerminal(os.Stdout),
		StdinTTY:   isTerminal(os.Stdin),
		Getenv:     os.Getenv,
		HomeDir:    os.UserHomeDir,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
	if a.StdinTTY {
		a.ReadSecret = func() (string, error) { return readHidden(os.Stdin) }
	}
	return a
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// color reports whether to write ANSI colors to stdout. NO_COLOR is
// honored (https://no-color.org).
func (a *App) color() bool { return a.StdoutTTY && a.Getenv("NO_COLOR") == "" }

func (a *App) userAgent() string {
	return fmt.Sprintf("hotcfg-cli/%s (%s/%s)", a.Version, runtime.GOOS, runtime.GOARCH)
}

type command struct {
	name    string
	args    string // positional synopsis
	summary string
	run     func(ctx context.Context, a *App, args []string) error
}

// commands is filled in init: the run functions look commands up by name
// (for their help text), which a package-level initializer cannot allow.
var commands []command

func init() {
	commands = []command{
		{"login", "", "Validate a secret API key and save it locally", runLogin},
		{"push", "<file.json>", "Upload a JSON file as the next version of a bin", runPush},
		{"get", "<bin_id>", "Print a bin's payload", runGet},
		{"diff", "<bin_id>", "Show a colored diff between two versions", runDiff},
		{"rollback", "<bin_id>", "Make an earlier version current again", runRollback},
		{"promote", "<bin_id>", "Copy the latest version of one environment to another", runPromote},
		{"history", "<bin_id>", "List a bin's versions in one environment", runHistory},
		{"unpublish", "<bin_id>", "Stop serving an environment, keeping its history", runUnpublish},
		{"bins list", "", "List your organization's bins", runBinsList},
		{"bins create", "<bin_id>", "Create a bin", runBinsCreate},
		{"env delete", "<bin_id>", "Delete an environment of a bin with its whole history", runEnvDelete},
		{"usage", "", "Show your organization's plan, usage and limits", runUsage},
	}
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// Run executes one command line (without the program name) and returns
// the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.usage(a.Stderr)
		return ExitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		if len(args) > 1 {
			return a.Run(ctx, append(args[1:], "--help"))
		}
		a.usage(a.Stdout)
		return ExitOK
	case "version", "--version":
		fmt.Fprintf(a.Stdout, "hotcfg %s\n", a.Version)
		return ExitOK
	}
	name, rest := args[0], args[1:]
	if isGroup(name) {
		if len(rest) == 0 || isHelpFlag(rest[0]) {
			w, code := a.Stdout, ExitOK
			if len(rest) == 0 {
				w, code = a.Stderr, ExitUsage
			}
			a.groupUsage(w, name)
			return code
		}
		name, rest = name+" "+rest[0], rest[1:]
	}
	c := findCommand(name)
	if c == nil {
		fmt.Fprintf(a.Stderr, "hotcfg: unknown command %q\n\n", name)
		a.usage(a.Stderr)
		return ExitUsage
	}
	err := c.run(ctx, a, rest)
	var ue *usageError
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, flag.ErrHelp):
		return ExitOK
	case errors.As(err, &ue):
		fmt.Fprintf(a.Stderr, "hotcfg %s: %s\n", c.name, ue.msg)
		fmt.Fprintf(a.Stderr, "Run 'hotcfg %s --help' for usage.\n", c.name)
		return ExitUsage
	case errors.Is(err, errReported):
		return ExitError
	default:
		fmt.Fprintf(a.Stderr, "error: %s\n", err)
		return ExitError
	}
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintf(w, "hotcfg manages hotcfg.run configuration bins.\n\nA <bin_id> is a bin's id (looked up in your key's organization) or org-slug/id.\n\nUsage:\n  hotcfg <command> [arguments] [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-12s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, `
Every command accepts:
  --key string      API key (default: $HOTCFG_API_KEY, then the saved credentials)
  --api-url string  API base URL (default: $HOTCFG_API_URL, then the saved
                    credentials, then %s)

Run 'hotcfg <command> --help' for a command's flags.
`, DefaultAPIURL)
}

func isHelpFlag(s string) bool {
	return s == "-h" || s == "-help" || s == "--help" || s == "help"
}

// usageError is a command-line mistake; Run prints it with a usage hint.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

// errReported means the command already printed its error.
var errReported = errors.New("error already reported")

// commonFlags are accepted by every command.
type commonFlags struct {
	key    string
	apiURL string
}

// newFlagSet builds a command's FlagSet with the common flags registered.
func (a *App) newFlagSet(c string) (*flag.FlagSet, *commonFlags) {
	cmd := findCommand(c)
	fs := flag.NewFlagSet(c, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are reported by Run; help by fs.Usage
	common := &commonFlags{}
	fs.StringVar(&common.key, "key", "", "API key (overrides $HOTCFG_API_KEY and saved credentials)")
	fs.StringVar(&common.apiURL, "api-url", "", "API base URL, e.g. http://localhost:8080")
	fs.Usage = func() {
		fmt.Fprintf(a.Stdout, "Usage: hotcfg %s", c)
		if cmd.args != "" {
			fmt.Fprintf(a.Stdout, " %s", cmd.args)
		}
		fmt.Fprintf(a.Stdout, " [flags]\n\n%s.\n\nFlags:\n", cmd.summary)
		printDefaults(a.Stdout, fs)
	}
	return fs, common
}

// printDefaults lists flags GNU-style ("--name value"), sorted.
func printDefaults(w io.Writer, fs *flag.FlagSet) {
	var lines [][2]string
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		left := "--" + f.Name
		if name != "" {
			left += " " + name
		}
		if f.DefValue != "" && f.DefValue != "false" {
			usage += fmt.Sprintf(" (default %q)", f.DefValue)
		}
		lines = append(lines, [2]string{left, usage})
	})
	sort.Slice(lines, func(i, j int) bool { return lines[i][0] < lines[j][0] })
	for _, l := range lines {
		fmt.Fprintf(w, "  %-22s %s\n", l[0], l[1])
	}
}

// parseArgs parses flags that may be interleaved with positional
// arguments. Everything after "--" is positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fs.Usage()
				return nil, err
			}
			return nil, &usageError{err.Error()}
		}
		rest := fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// exactArgs parses args and requires exactly n positional arguments, named
// by what (for the error message).
func exactArgs(fs *flag.FlagSet, args []string, what ...string) ([]string, error) {
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) < len(what) {
		return nil, usagef("missing %s", strings.Join(what[len(pos):], ", "))
	}
	if len(pos) > len(what) {
		return nil, usagef("unexpected argument %q", pos[len(what)])
	}
	return pos, nil
}
