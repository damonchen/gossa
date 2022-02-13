// export by github.com/goplus/gossa/cmd/qexp

//+build go1.15,!go1.16

package signal

import (
	q "os/signal"

	"reflect"

	"github.com/goplus/gossa"
)

func init() {
	gossa.RegisterPackage(&gossa.Package{
		Name: "signal",
		Path: "os/signal",
		Deps: map[string]string{
			"os":      "os",
			"sync":    "sync",
			"syscall": "syscall",
		},
		Interfaces: map[string]reflect.Type{},
		NamedTypes: map[string]gossa.NamedType{},
		AliasTypes: map[string]reflect.Type{},
		Vars:       map[string]reflect.Value{},
		Funcs: map[string]reflect.Value{
			"Ignore":  reflect.ValueOf(q.Ignore),
			"Ignored": reflect.ValueOf(q.Ignored),
			"Notify":  reflect.ValueOf(q.Notify),
			"Reset":   reflect.ValueOf(q.Reset),
			"Stop":    reflect.ValueOf(q.Stop),
		},
		TypedConsts:   map[string]gossa.TypedConst{},
		UntypedConsts: map[string]gossa.UntypedConst{},
	})
}