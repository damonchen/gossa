# gossa - Golang SSA interpreter

[![Go1.14](https://github.com/goplus/gossa/workflows/Go1.14/badge.svg)](https://github.com/goplus/gossa/actions?query=workflow%3AGo1.14)
[![Go1.15](https://github.com/goplus/gossa/workflows/Go1.15/badge.svg)](https://github.com/goplus/gossa/actions?query=workflow%3AGo1.15)
[![Go1.16](https://github.com/goplus/gossa/workflows/Go1.16/badge.svg)](https://github.com/goplus/gossa/actions?query=workflow%3AGo1.16)
[![Go1.17](https://github.com/goplus/gossa/workflows/Go1.17/badge.svg)](https://github.com/goplus/gossa/actions?query=workflow%3AGo1.17)
[![Go1.18](https://github.com/goplus/gossa/workflows/Go1.18/badge.svg)](https://github.com/goplus/gossa/actions?query=workflow%3AGo1.18)

### ABI

support ABI0 and ABIInternal

- ABI0 stack-based ABI
- ABIInternal [register-based Go calling convention proposal](https://golang.org/design/40724-register-calling)

	- Go1.17: amd64
	- Go1.18: amd64 arm64 ppc64/ppc64le

### unsupport features

- Go1.18 type parameters
- Go1.18 fuzzing

### gossa command line
```
go get -u github.com/goplus/gossa/cmd/gossa
```

Commands
```
gossa run         # interpret package
gossa test        # test package
```

### gossa package

**run go source**
```
package main

import (
	"github.com/goplus/gossa"
	_ "github.com/goplus/gossa/pkg/fmt"
)

var source = `
package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`

func main() {
	_, err := gossa.RunFile("main.go", source, nil, 0)
	if err != nil {
		panic(err)
	}
}

```

**run gop source**
```
package main

import (
	"github.com/goplus/gossa"
	_ "github.com/goplus/gossa/gopbuild"
	_ "github.com/goplus/gossa/pkg/fmt"
)

var source = `
println "Hello, Go+"
`

func main() {
	_, err := gossa.RunFile("main.gop", source, nil, 0)
	if err != nil {
		panic(err)
	}
}
```