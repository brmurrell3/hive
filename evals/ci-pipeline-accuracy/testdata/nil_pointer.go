// SPDX-License-Identifier: Apache-2.0
package main

import "fmt"

type User struct {
	Name  string
	Email string
}

func findUser(id int) *User {
	if id == 42 {
		return &User{Name: "Alice", Email: "alice@example.com"}
	}
	return nil
}

func main() {
	// BUG: No nil check before dereferencing the pointer.
	// findUser returns nil for any id != 42.
	user := findUser(1)
	fmt.Println(user.Name)
}
