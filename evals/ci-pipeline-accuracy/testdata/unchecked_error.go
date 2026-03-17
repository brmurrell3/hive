// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"os"
)

type Config struct {
	Port    int    `json:"port"`
	Host    string `json:"host"`
	Timeout int    `json:"timeout"`
}

func loadConfig(path string) Config {
	var cfg Config

	data, _ := os.ReadFile(path) // BUG: error ignored — file may not exist

	json.Unmarshal(data, &cfg) // BUG: error ignored — data may be invalid JSON

	return cfg // returns zero-value Config on any error, silently
}

func main() {
	cfg := loadConfig("config.json")
	_ = cfg
}
