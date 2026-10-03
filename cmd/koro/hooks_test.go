package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestWire_HooksUseOnlyAllowedTypes enforces BE-001 R4: hook signatures use
// only stdlib, platform/* and authz types (module types never enter hooks).
func TestWire_HooksUseOnlyAllowedTypes(t *testing.T) {
	t.Parallel()
	self := reflect.TypeFor[hooks]().PkgPath() // "koro/cmd/koro" in test binaries
	seen := map[reflect.Type]bool{}
	var check func(path string, ty reflect.Type)
	check = func(path string, ty reflect.Type) {
		if seen[ty] {
			return
		}
		seen[ty] = true
		pkg := ty.PkgPath()
		switch {
		case pkg == "" || pkg == self:
		case strings.Contains(strings.Split(pkg, "/")[0], "."):
			t.Errorf("%s uses third-party type %s", path, ty)
		case strings.HasPrefix(pkg, "koro/") && !strings.HasPrefix(pkg, "koro/internal/platform/") && pkg != "koro/internal/authz":
			t.Errorf("%s uses module type %s (adapt it in the consumer's wire file instead)", path, ty)
		}
		switch ty.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
			check(path, ty.Elem())
		case reflect.Map:
			check(path, ty.Key())
			check(path, ty.Elem())
		case reflect.Func:
			for i := range ty.NumIn() {
				check(path, ty.In(i))
			}
			for i := range ty.NumOut() {
				check(path, ty.Out(i))
			}
		case reflect.Struct:
			if pkg == self {
				for i := range ty.NumField() {
					check(path+"."+ty.Field(i).Name, ty.Field(i).Type)
				}
			}
		}
	}
	check("hooks", reflect.TypeFor[hooks]())
}
