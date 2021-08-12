// export by github.com/goplus/interp/cmd/qexp

// +build freebsd linux netbsd openbsd

package syscall

import (
	"syscall"

	"github.com/goplus/interp"
)

func init() {
	interp.RegisterPackage("syscall", extMap_3829281923, typList_3829281923)
}

var extMap_3829281923 = map[string]interface{}{
	"syscall.Accept4":   syscall.Accept4,
	"syscall.Nanosleep": syscall.Nanosleep,
	"syscall.Pipe2":     syscall.Pipe2,
}

var typList_3829281923 = []interface{}{}