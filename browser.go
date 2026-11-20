package main

import (
	"os"
	"os/exec"
	"runtime"
)

// openBrowser tries to open a URL, quietly: the URL is printed anyway, and on
// a server there may be no browser at all. POLYPACK_NO_BROWSER turns it off.
func openBrowser(url string) {
	if os.Getenv("POLYPACK_NO_BROWSER") != "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
