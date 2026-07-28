// Package config provides .env loading and typed environment-variable
// lookups, mirroring the Rust code's `dotenvy::dotenv()` + `std::env::var`
// idiom used throughout every binary.
package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// LoadDotenv loads KEY=VALUE pairs from a .env file in the current directory
// (if present) into the process environment, without overriding variables
// already set. Missing file is not an error — same as dotenvy::dotenv().
func LoadDotenv() {
	loadDotenvFile(".env")
}

func loadDotenvFile(path string) {
	f, err := os.Open(path)
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
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

// EnvOr returns the environment variable's value, or `def` if unset/empty.
func EnvOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// EnvFloat returns the env var parsed as float64, or `def` on missing/bad value.
func EnvFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// EnvInt returns the env var parsed as int, or `def` on missing/bad value.
func EnvInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// EnvUint64 returns the env var parsed as uint64, or `def` on missing/bad value.
func EnvUint64(key string, def uint64) uint64 {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// EnvBool returns true if the env var is set to a truthy value ("1", "true",
// "yes", case-insensitive); returns `def` if unset.
func EnvBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// MustEnv returns the env var's value or panics — for required settings
// (mirrors Rust's `.expect("set X in .env")`).
func MustEnv(key, hint string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		panic("config: set " + key + " in .env" + hintSuffix(hint))
	}
	return v
}

func hintSuffix(hint string) string {
	if hint == "" {
		return ""
	}
	return " (" + hint + ")"
}

// EnvOptional returns (value, true) if set and non-empty, else ("", false).
func EnvOptional(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
