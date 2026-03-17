// SPDX-License-Identifier: Apache-2.0
package main

import (
	"io"
	"net/http"
	"os"
)

func serveFile(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("file")

	// BUG: Path traversal — user-controlled filename is passed directly to
	// os.Open without sanitization. An attacker can request:
	//   ?file=../../../etc/passwd
	f, err := os.Open("/var/data/" + filename)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	defer f.Close()

	io.Copy(w, f)
}
