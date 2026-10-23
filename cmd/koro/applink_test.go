package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"koro/internal/platform/applink"
)

// appLinkConfirm maps every e-mail token link path (internal/platform/applink)
// to the API endpoint its client posts the token to (04 §2.1, §2.2; BE-096).
var appLinkConfirm = map[applink.TokenPath]string{
	applink.VerifyEmail:        "/api/v1/auth/email-verifications/confirm",
	applink.ResetPassword:      "/api/v1/auth/password-resets/confirm",
	applink.ConfirmEmailChange: "/api/v1/auth/email-change/confirm",
	applink.VerifyEdu:          "/api/v1/auth/edu-verifications/confirm",
	applink.AcceptInvitation:   "/api/v1/invitations/accept",
}

// sideEffectFreeTokenGets are the only GET operations that take a token:
// they read, never consume (BE-062: the unsubscribe page data; the POST of
// the same URL withdraws).
var sideEffectFreeTokenGets = map[string]bool{"getUnsubscribeLink": true}

// TestOpenAPI_AppLinkTokensConsumedOnlyByPost (BE-096): every token link
// path has a POST confirm endpoint whose JSON body carries "token", and no
// GET operation takes a token parameter (a link preview bot or a mail
// scanner that opens the link consumes nothing).
func TestOpenAPI_AppLinkTokensConsumedOnlyByPost(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "openapi.yaml")
	if err := runOpenAPI([]string{"-o", out}, io.Discard); err != nil {
		t.Fatalf("runOpenAPI() error = %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths      map[string]map[string]specOperation `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
				Required   []string       `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}

	if len(appLinkConfirm) != len(applink.TokenPaths()) {
		t.Fatalf("appLinkConfirm covers %d of %d token paths", len(appLinkConfirm), len(applink.TokenPaths()))
	}
	for _, p := range applink.TokenPaths() {
		endpoint, ok := appLinkConfirm[p]
		if !ok {
			t.Errorf("token path %s has no confirm endpoint", p)
			continue
		}
		post, ok := spec.Paths[endpoint]["post"]
		if !ok {
			t.Errorf("%s → POST %s missing from the spec", p, endpoint)
			continue
		}
		ref := post.RequestBody.Content["application/json"].Schema.Ref
		schema, ok := spec.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
		if !ok || schema.Properties["token"] == nil || !slices.Contains(schema.Required, "token") {
			t.Errorf("POST %s: request body %q must require a token", endpoint, ref)
		}
	}

	for path, ops := range spec.Paths {
		get, ok := ops["get"]
		if !ok || sideEffectFreeTokenGets[get.OperationID] {
			continue
		}
		for _, prm := range get.Parameters {
			if strings.Contains(strings.ToLower(prm.Name), "token") && prm.In != "header" {
				t.Errorf("GET %s (%s) takes %s parameter %q: e-mail tokens are consumed only by POST", path, get.OperationID, prm.In, prm.Name)
			}
		}
	}
}

type specOperation struct {
	OperationID string `yaml:"operationId"`
	Parameters  []struct {
		Name string `yaml:"name"`
		In   string `yaml:"in"`
	} `yaml:"parameters"`
	RequestBody struct {
		Content map[string]struct {
			Schema struct {
				Ref string `yaml:"$ref"`
			} `yaml:"schema"`
		} `yaml:"content"`
	} `yaml:"requestBody"`
}
