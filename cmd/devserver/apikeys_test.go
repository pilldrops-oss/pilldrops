package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestAPIKeyOwnerBinding: a minted key resolves to its owner, and an unknown
// token resolves to "" so ownerID falls back to the apiKeyOwner hash.
func TestAPIKeyOwnerBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)

	k, err := ks.Create("ci key", "u:user_9")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.Key == "" {
		t.Fatal("create response missing full key (shown once)")
	}
	if owner := ks.OwnerOf(k.Key); owner != "u:user_9" {
		t.Fatalf("OwnerOf(full key) = %q, want u:user_9", owner)
	}
	if owner := ks.OwnerOf("sf_totally-unknown-token"); owner != "" {
		t.Fatalf("OwnerOf(unknown token) = %q, want empty", owner)
	}
}

// TestAPIKeyListScoped: a caller sees and deletes only their own keys, and
// never the full key.
func TestAPIKeyListScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)

	a, err := ks.Create("a-key", "u:alice")
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := ks.Create("b-key", "u:bob")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	listA := ks.List("u:alice")
	if len(listA) != 1 || listA[0].ID != a.ID {
		t.Fatalf("List(alice) = %+v, want exactly alice's key", listA)
	}
	if listA[0].Key != "" {
		t.Fatal("List must never include the full key")
	}
	for _, k := range listA {
		if k.ID == b.ID {
			t.Fatal("alice's key list leaked bob's key")
		}
	}

	// Bob cannot delete Alice's key by id.
	if ok, err := ks.Delete(a.ID, "u:bob"); ok || err != nil {
		t.Fatalf("bob delete alice's key = (%v, %v), want (false, nil)", ok, err)
	}
	if ks.OwnerOf(a.Key) != "u:alice" {
		t.Fatal("alice's key must still be valid after bob's failed delete")
	}

	// Alice can delete her own key.
	if ok, err := ks.Delete(a.ID, "u:alice"); !ok || err != nil {
		t.Fatalf("alice delete her own key = (%v, %v), want (true, nil)", ok, err)
	}
	if ks.OwnerOf(a.Key) != "" {
		t.Fatal("alice's key must be gone after she deletes it")
	}
}

// TestAPIKeyPersistence: keys survive a reload and the raw key never hits disk.
func TestAPIKeyPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	k, err := LoadAPIKeys(path).Create("persisted", "u:carol")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ks2 := LoadAPIKeys(path) // simulates a portal restart
	if owner := ks2.OwnerOf(k.Key); owner != "u:carol" {
		t.Fatalf("after reload OwnerOf = %q, want u:carol", owner)
	}
	if !ks2.Validate(k.Key) {
		t.Fatal("after reload, key must still validate")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), k.Key) {
		t.Fatal("state file contains the raw key")
	}
}

// TestAPIKeyDevKeyNeverListedOrDeletable: the seeded dev key (owner "") never
// appears in a list and cannot be deleted through the API.
func TestAPIKeyDevKeyNeverListedOrDeletable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)
	const devKey = "sf_devkeyfortest0000000000000000"
	ks.seedDevKey(devKey)

	if list := ks.List(""); len(list) != 0 {
		t.Fatalf("List(\"\") = %+v, want empty (dev key must never be listed)", list)
	}
	if ok, _ := ks.Delete("dev", ""); ok {
		t.Fatal("dev key was deletable via an empty-owner Delete call")
	}
	if !ks.Validate(devKey) {
		t.Fatal("dev key must still validate for the auth gate after seeding")
	}
}

// TestAPIKeyJSONMatchesCanonicalContract: the wire shape shared with the SPA and
// fleet uses "masked_key", and a never-used key has an empty last_used.
func TestAPIKeyJSONMatchesCanonicalContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)

	if _, err := ks.Create("ci key", "u:user_9"); err != nil {
		t.Fatalf("create: %v", err)
	}
	list := ks.List("u:user_9")
	if len(list) != 1 {
		t.Fatalf("List = %+v, want exactly one key", list)
	}

	raw, err := json.Marshal(list[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"masked_key"`) {
		t.Fatalf("json = %s, want a \"masked_key\" field", body)
	}
	if strings.Contains(body, `"key_masked"`) {
		t.Fatalf("json = %s, must NOT contain the old \"key_masked\" field", body)
	}
	if list[0].LastUsed != "" {
		t.Fatalf("LastUsed = %q for a never-used key, want empty string", list[0].LastUsed)
	}
	if !strings.Contains(body, `"last_used":""`) {
		t.Fatalf("json = %s, want last_used to serialize as an empty string", body)
	}
}

// TestAPIKeySaveExcludesDevKeyFromDisk: the dev key is re-derived from
// PORTAL_API_KEY every boot and must never be persisted.
func TestAPIKeySaveExcludesDevKeyFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)
	ks.seedDevKey("sf_devkeyfortest0000000000000000")
	if _, err := ks.Create("customer key", "u:user_1"); err != nil {
		t.Fatalf("create: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal persisted file: %v", err)
	}
	if _, ok := onDisk["dev"]; ok {
		t.Fatalf("persisted file contains the dev key entry: %s", raw)
	}
	if len(onDisk) != 1 {
		t.Fatalf("persisted file = %+v, want exactly the one customer key", onDisk)
	}
}

// TestAPIKeySeedDevKeyRevokesOldHashOnRotation: re-seeding with a new dev key
// must revoke the old hash.
func TestAPIKeySeedDevKeyRevokesOldHashOnRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)
	const h1 = "sf_devkey_h1_0000000000000000000000"
	const h2 = "sf_devkey_h2_1111111111111111111111"

	ks.seedDevKey(h1)
	if !ks.Validate(h1) {
		t.Fatal("h1 must validate right after seeding")
	}

	ks.seedDevKey(h2) // simulates a PORTAL_API_KEY rotation across a restart
	if ks.Validate(h1) {
		t.Fatal("h1 (the OLD dev key) must no longer validate after rotation")
	}
	if !ks.Validate(h2) {
		t.Fatal("h2 (the NEW dev key) must validate")
	}
}

// unwritableStore returns a store whose path is a directory, so save()'s
// rename fails deterministically.
func unwritableStore(t *testing.T) *APIKeyStore {
	t.Helper()
	dir := t.TempDir()
	return LoadAPIKeys(dir) // the read error is treated as a first boot
}

// TestAPIKeyCreateRollsBackOnSaveFailure: a key whose persist fails must not
// stay live in memory.
func TestAPIKeyCreateRollsBackOnSaveFailure(t *testing.T) {
	ks := unwritableStore(t)

	k, err := ks.Create("ci key", "u:user_1")
	if err == nil {
		t.Fatal("Create over an unwritable path must return an error")
	}
	if k != nil {
		t.Fatalf("Create must not return a key on failure, got %+v", k)
	}
	if ks.Validate("sf_whatever") {
		t.Fatal("no key should validate when Create failed to persist")
	}
	if len(ks.List("u:user_1")) != 0 {
		t.Fatal("a key that failed to persist must not appear in List")
	}
}

// TestAPIKeyDeleteRollsBackOnSaveFailure: a delete whose persist fails is rolled
// back and reported as an error, not as not-found.
func TestAPIKeyDeleteRollsBackOnSaveFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)
	k, err := ks.Create("ci key", "u:user_1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Make the path unwritable after the key is persisted so Delete's save() fails.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir over the key file path: %v", err)
	}

	ok, delErr := ks.Delete(k.ID, "u:user_1")
	if delErr == nil {
		t.Fatal("Delete must report an error when save() fails")
	}
	if ok {
		t.Fatal("Delete must report false when save() fails")
	}
	if ks.OwnerOf(k.Key) != "u:user_1" {
		t.Fatal("key must still be valid after a failed delete was rolled back")
	}
	if len(ks.List("u:user_1")) != 1 {
		t.Fatal("key must still appear in List after a failed delete was rolled back")
	}
}

// TestAPIKeyDeleteNotFoundDistinctFromSaveFailure: a missing id reports
// ok=false, err=nil.
func TestAPIKeyDeleteNotFoundDistinctFromSaveFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)
	ok, err := ks.Delete("does-not-exist", "u:user_1")
	if ok {
		t.Fatal("deleting a nonexistent id must report false")
	}
	if err != nil {
		t.Fatalf("deleting a nonexistent id must not report an error, got %v", err)
	}
}

// TestLoadAPIKeysNullFileDoesNotPanic: a `null` file unmarshals to a nil map,
// and the first insert would panic.
func TestLoadAPIKeysNullFileDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	ks := LoadAPIKeys(path) // must not panic
	ks.seedDevKey("sf_devkeyfortest0000000000000000")
	if !ks.Validate("sf_devkeyfortest0000000000000000") {
		t.Fatal("dev key must validate after seeding into a null-loaded store")
	}
	if _, err := ks.Create("ci key", "u:user_1"); err != nil {
		t.Fatalf("create into a null-loaded store: %v", err)
	}
}

// TestLoadAPIKeysNilEntryDoesNotPanic: {"abc": null} yields a nil *APIKey that
// List/Delete/Touch would dereference.
func TestLoadAPIKeysNilEntryDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	if err := os.WriteFile(path, []byte(`{"abc":null}`), 0o600); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	ks := LoadAPIKeys(path) // must not panic
	if list := ks.List("u:whoever"); len(list) != 0 {
		t.Fatalf("List = %+v, want empty (the nil entry must have been dropped)", list)
	}
	if ok, _ := ks.Delete("abc", "u:whoever"); ok {
		t.Fatal("Delete of the dropped nil entry must report false")
	}
	ks.Touch("sf_whatever") // must not panic
}

// TestAPIKeyRoutesScopedByOwnerHTTP drives the /v1/keys handlers through chi
// with a header-based ownerID stub.
func TestAPIKeyRoutesScopedByOwnerHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys.json")
	ks := LoadAPIKeys(path)
	ks.seedDevKey("sf_devkeyfortest0000000000000000")

	aKey, err := ks.Create("alice's key", "u:alice")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := ks.Create("bob's key", "u:bob"); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	fakeOwnerID := func(r *http.Request) string { return r.Header.Get("X-Test-Owner") }
	r := chi.NewRouter()
	registerAPIKeyRoutes(r, ks, fakeOwnerID)

	get := func(owner string) []APIKey {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/keys", nil)
		req.Header.Set("X-Test-Owner", owner)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("GET /v1/keys as %s = %d body=%s", owner, rr.Code, rr.Body.String())
		}
		var list []APIKey
		if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return list
	}
	assertNoDevKey := func(list []APIKey, who string) {
		for _, k := range list {
			if k.Name == "portal-default" {
				t.Fatalf("dev key leaked into %s's list", who)
			}
		}
	}

	aliceList := get("u:alice")
	if len(aliceList) != 1 || aliceList[0].ID != aKey.ID {
		t.Fatalf("alice's list = %+v, want exactly her key", aliceList)
	}
	assertNoDevKey(aliceList, "alice")

	bobList := get("u:bob")
	if len(bobList) != 1 || bobList[0].ID == aKey.ID {
		t.Fatalf("bob's list = %+v, want exactly his own key (not alice's)", bobList)
	}
	assertNoDevKey(bobList, "bob")

	// Bob cannot delete Alice's key.
	req := httptest.NewRequest("DELETE", "/v1/keys/"+aKey.ID, nil)
	req.Header.Set("X-Test-Owner", "u:bob")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Fatalf("bob deleting alice's key = %d, want 404", rr.Code)
	}
	if len(get("u:alice")) != 1 {
		t.Fatal("alice's key must still exist after bob's failed delete")
	}

	// Alice can delete her own key.
	req = httptest.NewRequest("DELETE", "/v1/keys/"+aKey.ID, nil)
	req.Header.Set("X-Test-Owner", "u:alice")
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != 204 {
		t.Fatalf("alice deleting her own key = %d, want 204", rr.Code)
	}
	if len(get("u:alice")) != 0 {
		t.Fatal("alice's key must be gone after she deletes it")
	}
}
