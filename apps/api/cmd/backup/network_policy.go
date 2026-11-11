package main

import (
	"errors"
	"os"
	"strings"

	"lanqin-email-api/internal/backup/storage"
)

// Only an explicit operator flag selects an environment variable. Destination
// JSON and browser/API requests cannot supply or enlarge this deployment policy.
func backupNetworkPolicyFromEnv(reference string) (*storage.NetworkPolicy, error) {
	if reference == "" {
		return nil, nil
	}
	invalid := errors.New("backup network policy requires an explicit valid environment reference containing a strict host/port/private-IP allowlist")
	if !nativeDSNEnvPattern.MatchString(reference) {
		return nil, invalid
	}
	value, ok := os.LookupEnv(reference)
	if !ok || len(value) == 0 || len(value) > 64<<10 {
		return nil, invalid
	}
	policy, err := storage.DecodeNetworkPolicy(strings.NewReader(value))
	if err != nil {
		return nil, invalid
	}
	return policy, nil
}
