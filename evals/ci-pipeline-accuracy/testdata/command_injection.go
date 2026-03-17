// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"net/http"
	"os/exec"
)

func handlePing(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")

	// BUG: Command injection — user input is passed directly to a shell command.
	// An attacker can submit: ?host=127.0.0.1;cat /etc/passwd
	cmd := exec.Command("sh", "-c", "ping -c 1 "+host)
	output, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, "ping failed", 500)
		return
	}

	fmt.Fprintln(w, string(output))
}
