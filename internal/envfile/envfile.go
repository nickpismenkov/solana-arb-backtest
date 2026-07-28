// Package envfile is a minimal dotenv loader (port of the `dotenvy` crate
// usage: `let _ = dotenvy::dotenv();` — best-effort, ignored if missing).
package envfile

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv loads key=value pairs from a .env file in the current directory
// into the process environment, without overwriting variables already set.
// Missing file or parse errors are silently ignored, matching the original
// `let _ = dotenvy::dotenv();`.
func LoadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}
