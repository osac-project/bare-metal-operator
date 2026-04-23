/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lock

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"sigs.k8s.io/yaml"
)

var (
	_ Locker        = (*redisLocker)(nil)
	_ NewLockerFunc = NewLockerFunc(NewRedisLocker)
)

func init() {
	newLockerFuncs["redis"] = NewRedisLocker
}

// redisLocker provides distributed locking using Redis
type redisLocker struct {
	client       *redis.Client
	ttl          time.Duration
	unlockScript *redis.Script
}

var (
	// Lua script for atomic unlock: only delete if token matches
	unlockScriptSrc = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
else
	return 0
end
`
)

// redisOptionsCfg is an intermediate struct for parsing redis configuration
// with string fields for durations that need to be parsed
type redisOptionsCfg struct {
	Addr         string `json:"addr"`
	DB           int    `json:"db"`
	Password     string `json:"password"`
	DialTimeout  string `json:"dialTimeout"`
	ReadTimeout  string `json:"readTimeout"`
	WriteTimeout string `json:"writeTimeout"`
	IdleTimeout  string `json:"idleTimeout"`
	PoolSize     int    `json:"poolSize"`
	MinIdleConns int    `json:"minIdleConns"`
	MaxIdleConns int    `json:"maxIdleConns"`
}

// NewRedisLocker creates a new Redis-based distributed locker
func NewRedisLocker(ctx context.Context, cfg *Config) (Locker, error) {
	opts := cfg.Options

	// Parse options via intermediate struct to properly handle duration strings
	var optsCfg redisOptionsCfg
	if len(opts) > 0 {
		optsYAML, err := yaml.Marshal(opts)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal redis options: %w", err)
		}

		// Unmarshal into intermediate struct with strict mode to catch unknown fields
		if err := yaml.UnmarshalStrict(optsYAML, &optsCfg); err != nil {
			return nil, fmt.Errorf("failed to unmarshal redis options (unknown or invalid fields): %w", err)
		}
	}

	// Set defaults
	if optsCfg.Addr == "" {
		optsCfg.Addr = "localhost:6379"
	}

	// Build redis.Options with parsed durations
	redisOpts := redis.Options{
		Addr:         optsCfg.Addr,
		DB:           optsCfg.DB,
		Password:     optsCfg.Password,
		PoolSize:     optsCfg.PoolSize,
		MinIdleConns: optsCfg.MinIdleConns,
		MaxIdleConns: optsCfg.MaxIdleConns,
	}

	// Parse duration strings
	if optsCfg.DialTimeout != "" {
		d, err := time.ParseDuration(optsCfg.DialTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid dialTimeout format %q: %w", optsCfg.DialTimeout, err)
		}
		redisOpts.DialTimeout = d
	}
	if optsCfg.ReadTimeout != "" {
		d, err := time.ParseDuration(optsCfg.ReadTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid readTimeout format %q: %w", optsCfg.ReadTimeout, err)
		}
		redisOpts.ReadTimeout = d
	}
	if optsCfg.WriteTimeout != "" {
		d, err := time.ParseDuration(optsCfg.WriteTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid writeTimeout format %q: %w", optsCfg.WriteTimeout, err)
		}
		redisOpts.WriteTimeout = d
	}
	if optsCfg.IdleTimeout != "" {
		d, err := time.ParseDuration(optsCfg.IdleTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid idleTimeout format %q: %w", optsCfg.IdleTimeout, err)
		}
		redisOpts.ConnMaxIdleTime = d
	}

	redisClient := redis.NewClient(&redisOpts)

	// Test Redis connection
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis at %s: %w", redisOpts.Addr, err)
	}

	// Parse TTL duration from config
	ttl := 30 * time.Second // default
	if cfg.TTL != "" {
		parsedTTL, err := time.ParseDuration(cfg.TTL)
		if err != nil {
			return nil, fmt.Errorf("invalid ttl format: %w", err)
		}
		ttl = parsedTTL
	}

	return &redisLocker{
		client:       redisClient,
		ttl:          ttl,
		unlockScript: redis.NewScript(unlockScriptSrc),
	}, nil
}

// TryLock attempts to acquire a lock for the given key
// Returns (acquired bool, token string, error)
// If acquired is true, the token must be saved and used for Unlock
func (l *redisLocker) TryLock(ctx context.Context, key string) (bool, string, error) {
	lockKey := fmt.Sprintf("node-lock:%s", key)

	// Generate unique token for this lock acquisition
	token := uuid.New().String()

	// Use SET with NX (set if not exists) and expiration
	// Store the token as the value for ownership verification
	result, err := l.client.SetArgs(ctx, lockKey, token, redis.SetArgs{
		Mode: "NX",
		TTL:  l.ttl,
	}).Result()
	if err != nil {
		// redis.Nil means the key already exists (lock not acquired)
		if err == redis.Nil {
			return false, "", nil
		}
		return false, "", fmt.Errorf("failed to acquire lock for key %s: %w", key, err)
	}

	// SetArgs with NX returns "OK" if the key was set, empty string if it already exists
	if result == "OK" {
		return true, token, nil
	}
	return false, "", nil
}

// Unlock releases the lock for the given key
// The token must match the one returned by TryLock
func (l *redisLocker) Unlock(ctx context.Context, key string, token string) error {
	lockKey := fmt.Sprintf("node-lock:%s", key)

	// Use Lua script to atomically verify token and delete
	result, err := l.unlockScript.Run(ctx, l.client, []string{lockKey}, token).Result()
	if err != nil {
		return fmt.Errorf("failed to release lock for key %s: %w", key, err)
	}

	// Script returns 0 if token didn't match or key doesn't exist
	if result == int64(0) {
		return fmt.Errorf("lock for key %s does not exist or token mismatch", key)
	}

	return nil
}
