// Package config loads all settings from environment variables and fails fast.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

type Config struct {
	Port              string
	DatabaseURL       string
	RedisURL          string
	SupabaseURL       string
	SupabaseAnonKey   string
	AdminEmail        string
}

// Load reads env vars. If any required one is missing it returns an error
// naming ALL missing variables at once, so you fix them in one go.
func Load() (*Config, error) {
	c := &Config{
		Port:              os.Getenv("PORT"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		RedisURL:          os.Getenv("REDIS_URL"),
		SupabaseURL:       strings.TrimRight(os.Getenv("SUPABASE_URL"), "/"),
		SupabaseAnonKey:   os.Getenv("SUPABASE_ANON_KEY"),
		AdminEmail:        strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL"))),
	}
	var missing []string
	for name, v := range map[string]string{
		"PORT": c.Port, "DATABASE_URL": c.DatabaseURL, "REDIS_URL": c.RedisURL,
		"SUPABASE_URL": c.SupabaseURL,
		"SUPABASE_ANON_KEY": c.SupabaseAnonKey, "ADMIN_EMAIL": c.AdminEmail,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(c.RedisURL, "redis://") && !strings.HasPrefix(c.RedisURL, "rediss://") {
		return nil, fmt.Errorf("REDIS_URL must start with rediss:// (Upstash TCP URL, not the REST URL)")
	}
	return c, nil
}
