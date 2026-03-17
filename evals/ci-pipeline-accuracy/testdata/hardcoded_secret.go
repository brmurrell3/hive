// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"net/http"
)

const (
	// BUG: Hardcoded secret — API key embedded directly in source code.
	// This should be loaded from environment variables or a secrets manager.
	apiKey    = "sk-live-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6"
	apiSecret = "whsec_MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQ"
)

func callExternalAPI() {
	req, _ := http.NewRequest("GET", "https://api.example.com/data", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Webhook-Secret", apiSecret)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println("status:", resp.StatusCode)
}
