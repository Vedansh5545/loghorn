// Package browser opens a URL in the user's default browser.
package browser

import (
	"os/exec"
	"runtime"
)

// Open starts the platform's opener for url and returns once it has started;
// it does not wait for the browser.
func Open(url string) error {
	cmd := command(runtime.GOOS, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap it
	return nil
}

func command(goos, url string) *exec.Cmd {
	switch goos {
	case "darwin":
		return exec.Command("open", url)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	}
	return exec.Command("xdg-open", url)
}
