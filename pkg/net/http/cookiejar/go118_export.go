// export by github.com/goplus/gossa/cmd/qexp

//go:build go1.18
// +build go1.18

package cookiejar

import (
	q "net/http/cookiejar"

	"reflect"

	"github.com/goplus/gossa"
)

func init() {
	gossa.RegisterPackage(&gossa.Package{
		Name: "cookiejar",
		Path: "net/http/cookiejar",
		Deps: map[string]string{
			"errors":                  "errors",
			"fmt":                     "fmt",
			"net":                     "net",
			"net/http":                "http",
			"net/http/internal/ascii": "ascii",
			"net/url":                 "url",
			"sort":                    "sort",
			"strings":                 "strings",
			"sync":                    "sync",
			"time":                    "time",
			"unicode/utf8":            "utf8",
		},
		Interfaces: map[string]reflect.Type{
			"PublicSuffixList": reflect.TypeOf((*q.PublicSuffixList)(nil)).Elem(),
		},
		NamedTypes: map[string]gossa.NamedType{
			"Jar":     {reflect.TypeOf((*q.Jar)(nil)).Elem(), "", "Cookies,SetCookies,cookies,domainAndType,newEntry,setCookies"},
			"Options": {reflect.TypeOf((*q.Options)(nil)).Elem(), "", ""},
		},
		AliasTypes: map[string]reflect.Type{},
		Vars:       map[string]reflect.Value{},
		Funcs: map[string]reflect.Value{
			"New": reflect.ValueOf(q.New),
		},
		TypedConsts:   map[string]gossa.TypedConst{},
		UntypedConsts: map[string]gossa.UntypedConst{},
	})
}
