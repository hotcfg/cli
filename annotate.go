package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// GitHub Actions annotations. When GITHUB_ACTIONS=true (set on every GitHub
// runner) and --no-annotations is not given, failures are also printed as
// workflow commands ("::error file=f.json,line=3::message"), which GitHub
// shows on the line of the file in the pull request. They go to stderr, so
// stdout stays clean for "hotcfg get > file.json" and --json.
//
// https://docs.github.com/actions/reference/workflow-commands-for-github-actions

func (a *App) annotations() bool {
	return !a.noAnnotations && a.Getenv != nil && a.Getenv("GITHUB_ACTIONS") == "true"
}

// annotation is one ::error command. file, line and col are optional.
type annotation struct {
	file      string
	line, col int
	message   string
}

func (a *App) annotate(an annotation) {
	if !a.annotations() {
		return
	}
	var props []string
	if an.file != "" {
		props = append(props, "file="+escapeProperty(an.file))
		if an.line > 0 {
			props = append(props, "line="+strconv.Itoa(an.line))
			if an.col > 0 {
				props = append(props, "col="+strconv.Itoa(an.col))
			}
		}
	}
	cmd := "::error"
	if len(props) > 0 {
		cmd += " " + strings.Join(props, ",")
	}
	fmt.Fprintf(a.Stderr, "%s::%s\n", cmd, escapeData(an.message))
}

// annotateError annotates a command's failure: a local JSON syntax error on
// its line, anything else as one message.
func (a *App) annotateError(err error) {
	var se *jsonSyntaxError
	if errors.As(err, &se) {
		a.annotate(annotation{file: se.file, line: se.line, col: se.col, message: "invalid JSON: " + se.msg})
		return
	}
	a.annotate(annotation{message: err.Error()})
}

// escapeData escapes a workflow command's message, so a newline in it
// cannot end the command and start another one.
func escapeData(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

// escapeProperty escapes a workflow command property value (file=...).
func escapeProperty(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C").Replace(s)
}

// oneLine makes text from a file or the API safe to print on one line:
// control characters are shown escaped. A JSON key like "a\n::error::x"
// would otherwise start a line that CI systems read as a command.
func oneLine(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if isControl(r) {
			q := strconv.QuoteRune(r) // '\n', '\x1b', ...
			b.WriteString(q[1 : len(q)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }
