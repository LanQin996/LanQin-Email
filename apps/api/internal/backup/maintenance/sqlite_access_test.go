package maintenance

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSQLiteReadGroupRequiresExplicitNonRootGIDAndSQLite(t *testing.T) {
	cfg := faultConfig(t)
	original, err := ConfigHash(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(cfg)
	if bytes.Contains(data, []byte("sqliteGroupRead")) {
		t.Fatal("legacy plan encoding changed")
	}
	for _, change := range []string{"zero-group", "native"} {
		current := cfg
		current.SQLiteGroupRead = true
		if change == "native" {
			current.Driver = "postgres"
			current.Owners.Data.GID = 103
		}
		if ValidateConfig(current) == nil {
			t.Fatal("invalid read group policy accepted", change)
		}
	}
	cfg.SQLiteGroupRead, cfg.Owners.Data.GID = true, 103
	data, _ = json.Marshal(cfg)
	decoded, err := DecodeConfig(bytes.NewReader(data))
	if err != nil || !decoded.SQLiteGroupRead || decoded.Owners.Data.GID != 103 {
		t.Fatal("explicit SQLite read group lost", err)
	}
	hash, _ := ConfigHash(cfg)
	if hash == original {
		t.Fatal("reader policy not bound into plan hash")
	}
}

func TestComposePreflightProvesActualSQLiteReaderBeforeStopping(t *testing.T) {
	for _, scenario := range []string{"private-inaccessible", "wrong-group", "malformed-group", "reader-is-owner", "matching-read-group"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := faultConfig(t)
			cfg.SQLiteGroupRead, cfg.Owners.Data.GID = true, 103
			fake := &fakeComposeDocker{t: t, cfg: cfg, postfixUID: "101", postfixGroups: "103"}
			switch scenario {
			case "private-inaccessible":
				cfg.SQLiteGroupRead = false
			case "wrong-group":
				fake.postfixGroups = "104"
			case "malformed-group":
				fake.postfixGroups = "103 invalid"
			case "reader-is-owner":
				cfg.Owners.Data.UID = 101
			}
			fake.cfg = cfg
			fake.setDeployment("sqlite", composeDatabaseIdentity{})
			controller := fake.controller()
			err := controller.Preflight(t.Context(), cfg)
			if (err == nil) != (scenario == "matching-read-group") {
				t.Fatal("reader proof incorrect", scenario, err)
			}
			for _, call := range fake.calls {
				if len(call) > len(composePrefix(cfg)) && call[0] == "compose" && strings.Join(call[len(composePrefix(cfg)):], " ") == "stop --timeout 30 lanqin-email" {
					t.Fatal("preflight reader rejection stopped services")
				}
			}
		})
	}
}
