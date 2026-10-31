package main

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// pathTemplateParam is one {name} of a path template.
var pathTemplateParam = regexp.MustCompile(`\{([^{}]+)\}`)

// TestOpenAPI_PathParametersMatchTheTemplate (BE-154b): every operation of
// the full API declares exactly the path parameters its path template
// names, each required, once. huma reads the fields of an embedded struct
// only when its type is exported: an input that embeds an unexported path
// struct registers without its parameters, the contract (and a generated
// client) loses them and the handler reads them empty — a 404 on every
// call (replacePayeeBankAccountEvidence embedded orgPayeePath).
func TestOpenAPI_PathParametersMatchTheTemplate(t *testing.T) {
	t.Parallel()
	oapi := specApp(t)
	if len(oapi.Paths) == 0 {
		t.Fatal("the spec has no paths")
	}
	var bad []string
	for path, item := range oapi.Paths {
		var want []string
		for _, m := range pathTemplateParam.FindAllStringSubmatch(path, -1) {
			want = append(want, m[1])
		}
		slices.Sort(want)
		for method, o := range map[string]*huma.Operation{"GET": item.Get, "PUT": item.Put, "POST": item.Post,
			"DELETE": item.Delete, "OPTIONS": item.Options, "HEAD": item.Head, "PATCH": item.Patch, "TRACE": item.Trace} {
			if o == nil {
				continue
			}
			var got []string
			for _, p := range append(slices.Clone(item.Parameters), o.Parameters...) {
				if p == nil || p.In != "path" {
					continue
				}
				got = append(got, p.Name)
				if !p.Required {
					bad = append(bad, method+" "+path+" ("+o.OperationID+"): path parameter "+p.Name+" is not required")
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				bad = append(bad, method+" "+path+" ("+o.OperationID+"): path parameters ["+strings.Join(got, ", ")+
					"], the template names ["+strings.Join(want, ", ")+"]")
			}
		}
	}
	slices.Sort(bad)
	if len(bad) > 0 {
		t.Fatalf("path parameters that do not match their template:\n%s", strings.Join(bad, "\n"))
	}
}
