//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMeReturnsTheCallingPrincipalAndItsGroups(t *testing.T) {
	tr := start(t)
	aliceID := tr.admin(t, "principal", "create", "alice")
	legalID := tr.admin(t, "group", "create", "legal")
	tr.admin(t, "group", "add-member", "legal", "alice")
	_, key, _ := strings.Cut(tr.admin(t, "key", "create", "alice"), "\t")

	resp := tr.get(t, "/me", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me = %d, want 200", resp.StatusCode)
	}
	var me struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Groups []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if me.ID != aliceID || me.Name != "alice" {
		t.Errorf("principal = %s %q, want %s alice", me.ID, me.Name, aliceID)
	}
	if len(me.Groups) != 1 || me.Groups[0].ID != legalID || me.Groups[0].Name != "legal" {
		t.Errorf("groups = %+v, want only legal %s", me.Groups, legalID)
	}
}

func TestReadinessSeesPostgresAndNeo4j(t *testing.T) {
	tr := start(t)
	var ready struct {
		Checks map[string]struct{ Status string } `json:"checks"`
	}
	if err := json.NewDecoder(tr.get(t, "/readyz", "").Body).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	for _, dep := range []string{"postgres", "neo4j"} {
		if got := ready.Checks[dep].Status; got != "up" {
			t.Errorf("%s = %q, want up", dep, got)
		}
	}
}

func TestMeWithoutAValidKeyIs401(t *testing.T) {
	tr := start(t)
	for _, key := range []string{"", "trelis_not-a-key"} {
		if resp := tr.get(t, "/me", key); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET /me with key %q = %d, want 401", key, resp.StatusCode)
		}
	}
}
