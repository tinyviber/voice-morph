package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type dubberResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

func parseDubberResponse(t *testing.T, payload string) dubberResponse {
	t.Helper()
	var r dubberResponse
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		t.Fatalf("response is not the standard JSON envelope: %v (%q)", err, payload)
	}
	return r
}

func TestDubberLoadTextFileDistinguishesMissingFromReadFailure(t *testing.T) {
	d := &Dubber{}
	dir := t.TempDir()
	path := filepath.Join(dir, "voices.json")
	content := `{"version":1}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	var existing struct {
		Exists bool   `json:"exists"`
		Text   string `json:"text"`
	}
	r := parseDubberResponse(t, d.LoadTextFile(path))
	if !r.OK || json.Unmarshal(r.Data, &existing) != nil || !existing.Exists || existing.Text != content {
		t.Fatalf("existing file response = %+v, data=%s", r, r.Data)
	}

	var missing struct {
		Exists bool `json:"exists"`
	}
	r = parseDubberResponse(t, d.LoadTextFile(filepath.Join(dir, "missing.json")))
	if !r.OK || json.Unmarshal(r.Data, &missing) != nil || missing.Exists {
		t.Fatalf("missing file must be a successful, explicit absence: %+v, data=%s", r, r.Data)
	}

	r = parseDubberResponse(t, d.LoadTextFile(dir))
	if r.OK || r.Error == "" {
		t.Fatalf("read failure must remain an error, got %+v", r)
	}
}

func TestDubberCreateTextFileIfMissingDoesNotOverwrite(t *testing.T) {
	d := &Dubber{}
	path := filepath.Join(t.TempDir(), "voices.json")

	first := parseDubberResponse(t, d.CreateTextFileIfMissing(path, "first"))
	if !first.OK {
		t.Fatalf("create failed: %s", first.Error)
	}
	second := parseDubberResponse(t, d.CreateTextFileIfMissing(path, "replacement"))
	if second.OK {
		t.Fatal("second create unexpectedly replaced an existing file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("existing content overwritten: %q", got)
	}
}
