// SPDX-License-Identifier: Apache-2.0
package main

import (
	"database/sql"
	"fmt"
	"net/http"
)

func handleSearch(w http.ResponseWriter, r *http.Request) {
	db, _ := sql.Open("sqlite3", "app.db")
	defer db.Close()

	query := r.URL.Query().Get("q")

	// BUG: SQL injection — user input is interpolated directly into the query
	// string without parameterization. An attacker can submit:
	//   ?q=' OR 1=1; DROP TABLE users; --
	rows, err := db.Query("SELECT * FROM products WHERE name = '" + query + "'")
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()

	fmt.Fprintln(w, "results returned")
}
