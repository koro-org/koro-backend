// Package httpxtest holds the contract assertions of module HTTP tests
// (BE-168). It is imported only by tests.
package httpxtest

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// WantConforms fails the test unless body, a JSON response with status to
// the operation operationID of api, matches the schema the served OpenAPI
// document gives that response: what Schemathesis'
// response_schema_conformance check (zap.yml) asserts, caught before the
// weekly run. Required properties, additionalProperties false, types,
// enums and formats are checked the way huma validates (read from server).
func WantConforms(t testing.TB, api huma.API, operationID string, status int, body []byte) {
	t.Helper()
	oapi := api.OpenAPI()
	o := findOperation(oapi, operationID)
	if o == nil {
		t.Fatalf("no operation %s in the OpenAPI document", operationID)
	}
	resp := o.Responses[strconv.Itoa(status)]
	if resp == nil {
		resp = o.Responses["default"]
	}
	if resp == nil {
		t.Fatalf("%s documents no %d response", operationID, status)
	}
	s := jsonSchema(resp.Content)
	if s == nil {
		t.Fatalf("%s documents no JSON body for %d", operationID, status)
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%s %d: body is not JSON: %v", operationID, status, err)
	}
	res := &huma.ValidateResult{}
	huma.Validate(oapi.Components.Schemas, s, huma.NewPathBuffer(make([]byte, 0, 128), 0), huma.ModeReadFromServer, v, res)
	if len(res.Errors) > 0 {
		msgs := make([]string, len(res.Errors))
		for i, e := range res.Errors {
			msgs[i] = e.Error()
		}
		t.Fatalf("%s %d does not match its schema:\n  %s\nbody: %s", operationID, status, strings.Join(msgs, "\n  "), body)
	}
}

func findOperation(oapi *huma.OpenAPI, id string) *huma.Operation {
	for _, p := range oapi.Paths {
		for _, o := range []*huma.Operation{p.Get, p.Put, p.Post, p.Delete, p.Options, p.Head, p.Patch, p.Trace} {
			if o != nil && o.OperationID == id {
				return o
			}
		}
	}
	return nil
}

// jsonSchema is the schema of the JSON media type of a response
// (application/json, or a +json type such as application/problem+json).
func jsonSchema(content map[string]*huma.MediaType) *huma.Schema {
	if mt := content["application/json"]; mt != nil && mt.Schema != nil {
		return mt.Schema
	}
	for ct, mt := range content {
		if strings.HasSuffix(ct, "+json") && mt != nil && mt.Schema != nil {
			return mt.Schema
		}
	}
	return nil
}
