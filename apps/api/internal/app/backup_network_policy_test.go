package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"lanqin-email-api/internal/backup/storage"
)

const backupNetworkPolicyFixture = `{"version":1,"targets":[{"host":"192.168.1.10","port":8443,"ips":["192.168.1.10"]}]}`

func TestBackupNetworkPolicyOnlyDeploymentConfigCanAuthorizePrivateDestination(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey = backupTestKey
	client := backupLoggedInAdmin(t, a)
	list := backupList(t, client)
	req := backupTestRequest("webdav", "nas", list.Revision)
	req.WebDAV.URL = "https://192.168.1.10:8443/backups"
	var response map[string]any
	if status := client.do("POST", "/api/admin/backups/destinations", req, &response); status != http.StatusBadRequest {
		t.Fatal("private destination accepted without deployment allowlist", status)
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var injected map[string]any
	if json.Unmarshal(data, &injected) != nil {
		t.Fatal("invalid fixture")
	}
	injected["networkPolicy"] = json.RawMessage(backupNetworkPolicyFixture)
	if status := client.do("POST", "/api/admin/backups/destinations", injected, &response); status != http.StatusBadRequest {
		t.Fatal("Web request supplied its own network policy", status)
	}
	a.cfg.BackupNetworkAllowlistJSON = backupNetworkPolicyFixture
	if status := client.do("POST", "/api/admin/backups/destinations", req, &response); status != http.StatusCreated {
		t.Fatal("explicit deployment NAS destination rejected", status)
	}
	state, _, err := a.loadBackupDestinationState(t.Context())
	if err != nil || len(state.Items) != 1 || state.Items[0].Config.NetworkPolicy == nil {
		t.Fatal("deployment policy did not reach decrypted transfer config", err)
	}
	if err := (storage.Config{Version: 1, Destinations: []storage.Destination{state.Items[0].Config}}).Validate(); err != nil {
		t.Fatal("loaded private destination not usable", err)
	}
	portable, err := json.Marshal(state)
	if err != nil || strings.Contains(string(portable), "networkPolicy") || strings.Contains(string(portable), "\"ips\"") {
		t.Fatal("runtime deployment policy entered portable vault data", err)
	}
	view, _ := json.Marshal(backupList(t, client))
	if strings.Contains(string(view), "networkPolicy") || strings.Contains(string(view), "\"ips\"") {
		t.Fatal("deployment policy exposed in response")
	}
	a.cfg.BackupNetworkAllowlistJSON = ""
	state, _, err = a.loadBackupDestinationState(t.Context())
	if err != nil || state.Items[0].Config.NetworkPolicy != nil {
		t.Fatal("revoked deployment policy persisted in vault", err)
	}
	if err := (storage.Config{Version: 1, Destinations: []storage.Destination{state.Items[0].Config}}).Validate(); err == nil {
		t.Fatal("revoked NAS policy still authorized transfer")
	}
	a.cfg.BackupNetworkAllowlistJSON = `{"version":1,"targets":[],"do-not-echo":"synthetic-secret"}`
	if _, _, err := a.loadBackupDestinationState(t.Context()); !errors.Is(err, errBackupExecution) {
		t.Fatal("invalid deployment policy did not fail closed", err)
	}
	if status := client.do("GET", "/api/admin/backups/destinations", nil, &response); status != http.StatusServiceUnavailable || strings.Contains(jsonEncode(response), "synthetic-secret") {
		t.Fatal("invalid policy was ignored or echoed", status)
	}
}

func TestBackupNetworkPolicyInvalidEvenBeforeAnyDestinationIsSaved(t *testing.T) {
	a := newTestApp(t)
	a.cfg.BackupSecretKey, a.cfg.BackupNetworkAllowlistJSON = backupTestKey, `{"version":2,"targets":[]}`
	if _, _, err := a.loadBackupDestinationState(t.Context()); !errors.Is(err, errBackupExecution) {
		t.Fatal("malformed deployment policy silently treated as public default", err)
	}
}
