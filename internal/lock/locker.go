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

// Package lock provides implementations of distributed locking mechanisms
package lock

import (
	"context"
)

// Config is a struct that holds info needed to create a new locker implementation
type Config struct {
	Type    string         `json:"type"`
	TTL     string         `json:"ttl"`
	Options map[string]any `json:"options"`
}

// Locker defines the interface for distributed locking
type Locker interface {
	// TryLock attempts to acquire a lock for the given key
	// Returns (acquired bool, token string, error)
	// If acquired is true, token must be saved and used for Unlock
	TryLock(ctx context.Context, key string) (bool, string, error)

	// Unlock releases the lock for the given key
	// The token must match the one returned by TryLock
	Unlock(ctx context.Context, key string, token string) error
}

// NewLockerFunc is a function that creates a new locker from config
type NewLockerFunc func(ctx context.Context, cfg *Config) (Locker, error)

// newLockerFuncs is a registry of available locker implementations
var newLockerFuncs = make(map[string]NewLockerFunc)

// NewLocker creates a new locker based on the config type
func NewLocker(ctx context.Context, cfg *Config) (Locker, error) {
	newLockerFunc, ok := newLockerFuncs[cfg.Type]
	if !ok {
		return nil, nil
	}

	return newLockerFunc(ctx, cfg)
}
