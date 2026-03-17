// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"sync"
)

// Counter is a shared counter with a race condition.
type Counter struct {
	value int
}

func main() {
	c := &Counter{}
	var wg sync.WaitGroup

	// BUG: Multiple goroutines read and write c.value without synchronization.
	// This is a data race detectable with `go test -race`.
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.value++ // unsynchronized write
		}()
	}

	wg.Wait()
	fmt.Println("counter:", c.value) // unsynchronized read
}
