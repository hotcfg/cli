//go:build linux || darwin || freebsd

package cli

import (
	"bufio"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"
)

// readHidden reads one line from the terminal f with echo turned off.
func readHidden(f *os.File) (string, error) {
	fd := f.Fd()
	var old syscall.Termios
	if err := termios(fd, ioctlGetTermios, &old); err != nil {
		return readLine(f) // not a terminal after all
	}
	quiet := old
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ISIG
	if err := termios(fd, ioctlSetTermios, &quiet); err != nil {
		return readLine(f)
	}
	restore := func() { _ = termios(fd, ioctlSetTermios, &old) }

	// Ctrl-C must not leave the terminal without echo.
	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			restore()
			os.Stderr.WriteString("\n")
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		signal.Stop(sig)
		close(done)
		restore()
	}()
	return readLine(f)
}

func readLine(f *os.File) (string, error) {
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func termios(fd uintptr, req uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}
