// Copyright 2024 SandrPod
package poder

import (
	"encoding/json"
	"testing"
)

// TestCreatePodRequestJSONTags locks in the snake_case wire format
// expected by sandpod-server when it forwards a CreateSandboxRequest to
// the Poder. Without these tags Go's case-insensitive default matcher
// silently drops snake_case keys like "image_id" because they don't
// match "ImageID" (underscore-handling failure), letting the caller's
// custom image fall through to the default — which is exactly the bug
// this commit fixes.
//
// Each round-trip case below also doubles as a contract guard: if a
// future refactor renames a field or drops a tag, the resulting JSON
// string changes and this test fails fast, before the change ships and
// breaks deployments.
func TestCreatePodRequestJSONTags(t *testing.T) {
	cases := []struct {
		name     string
		req      CreatePodRequest
		wantJSON string // marshal output (deterministic field order)
	}{
		{
			name: "image_id flows through verbatim",
			req: CreatePodRequest{
				Name:    "d-test-abcd",
				ImageID: "example/sandrpod-toolbox-custom:latest",
			},
			wantJSON: `{"name":"d-test-abcd","image_id":"example/sandrpod-toolbox-custom:latest"}`,
		},
		{
			name: "all known fields use snake_case",
			req: CreatePodRequest{
				Name:         "d-full-fxgh",
				Region:       "cn-north-1",
				InstanceType: "small",
				ImageID:      "alpine:3.19",
				Provider:     "local",
				Labels:       map[string]string{"k": "v"},
				APIURL:       "http://toolbox:8080",
				PoderVersion: "1.0.0",
				LogLevel:     "info",
			},
			wantJSON: `{"name":"d-full-fxgh","region":"cn-north-1","instance_type":"small","image_id":"alpine:3.19","provider":"local","labels":{"k":"v"},"api_url":"http://toolbox:8080","poder_version":"1.0.0","log_level":"info"}`,
		},
		{
			name:     "empty struct produces only required name",
			req:      CreatePodRequest{},
			wantJSON: `{"name":""}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.wantJSON {
				t.Errorf(
					"marshal mismatch:\n  got:  %s\n  want: %s",
					string(got), tc.wantJSON,
				)
			}
		})
	}
}

// TestCreatePodRequestRoundTrip verifies the actual integration
// scenario: sandpod-server marshals upstream JSON (snake_case) and Poder
// unmarshals into CreatePodRequest. This is the path that was broken
// before the JSON tags were added — the upstream key "image_id" would
// not bind to the struct's ImageID field, leaving it as "".
func TestCreatePodRequestRoundTrip(t *testing.T) {
	// This payload simulates exactly what server forwards via the tunnel
	// after marshaling its own CreateSandboxRequest (which has json
	// tags). Poder must decode it without losing image_id.
	payload := []byte(`{
		"name":         "d-roundtrip-tcjz",
		"region":       "cn-north-1",
		"instance_type": "small",
		"image_id":     "example/sandrpod-toolbox-custom:test",
		"provider":     "local",
		"api_url":      "http://toolbox:8080",
		"poder_version": "1.2.3",
		"log_level":    "debug"
	}`)

	var req CreatePodRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The critical assertion — without json tags this would fail because
	// "image_id" doesn't match "ImageID" under Go's default rules.
	if got := req.ImageID; got != "example/sandrpod-toolbox-custom:test" {
		t.Errorf(
			"ImageID lost in round-trip: got %q (this is the bug — "+
				"snake_case key didn't bind to ImageID field)",
			got,
		)
	}

	// Sanity-check the rest of the fields too — broken tags on any one
	// of these would equally cause silent data loss.
	if req.Name != "d-roundtrip-tcjz" {
		t.Errorf("Name: got %q, want d-roundtrip-tcjz", req.Name)
	}
	if req.InstanceType != "small" {
		t.Errorf("InstanceType: got %q, want small", req.InstanceType)
	}
	if req.APIURL != "http://toolbox:8080" {
		t.Errorf("APIURL: got %q, want http://toolbox:8080", req.APIURL)
	}
	if req.PoderVersion != "1.2.3" {
		t.Errorf("PoderVersion: got %q, want 1.2.3", req.PoderVersion)
	}
	if req.LogLevel != "debug" {
		t.Errorf("LogLevel: got %q, want debug", req.LogLevel)
	}
}
