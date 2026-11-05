//go:build !(linux || darwin || freebsd)

package cli

import (
	"bufio"
	"os"
	"strings"
)

// readHidden reads one line. Hiding input is not implemented on this
// platform, so the key is echoed; pass --key or HOTCFG_API_KEY instead.
func readHidden(f *os.File) (string, error) {
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
