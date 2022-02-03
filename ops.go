// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gossa

import (
	"bytes"
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/goplus/reflectx"
	"golang.org/x/tools/go/ssa"
)

// If the target program panics, the interpreter panics with this type.
type targetPanic struct {
	v value
}

func (p targetPanic) Error() string {
	return toString(p.v)
}

// If the target program calls exit, the interpreter panics with this type.
type exitPanic int

// constValue returns the value of the constant with the
// dynamic type tag appropriate for c.Type().
func (fr *frame) constValue(c *ssa.Const) value {
	typ := c.Type()
	if t, ok := fr.typeParam[typ]; ok {
		typ = t
	}
	if c.IsNil() {
		return reflect.Zero(fr.i.toType(typ)).Interface()
		// return zero(c.Type()) // typed nil
	}
	if t, ok := typ.Underlying().(*types.Basic); ok {
		// TODO(adonovan): eliminate untyped constants from SSA form.
		switch t.Kind() {
		case types.Bool, types.UntypedBool:
			return constant.BoolVal(c.Value)
		case types.Int, types.UntypedInt:
			// Assume sizeof(int) is same on host and target.
			return int(c.Int64())
		case types.Int8:
			return int8(c.Int64())
		case types.Int16:
			return int16(c.Int64())
		case types.Int32, types.UntypedRune:
			return int32(c.Int64())
		case types.Int64:
			return c.Int64()
		case types.Uint:
			// Assume sizeof(uint) is same on host and target.
			return uint(c.Uint64())
		case types.Uint8:
			return uint8(c.Uint64())
		case types.Uint16:
			return uint16(c.Uint64())
		case types.Uint32:
			return uint32(c.Uint64())
		case types.Uint64:
			return c.Uint64()
		case types.Uintptr:
			// Assume sizeof(uintptr) is same on host and target.
			return uintptr(c.Uint64())
		case types.Float32:
			return float32(c.Float64())
		case types.Float64, types.UntypedFloat:
			return c.Float64()
		case types.Complex64:
			return complex64(c.Complex128())
		case types.Complex128, types.UntypedComplex:
			return c.Complex128()
		case types.String, types.UntypedString:
			if c.Value.Kind() == constant.String {
				return constant.StringVal(c.Value)
			}
			return string(rune(c.Int64()))
		case types.UnsafePointer:
			return unsafe.Pointer(uintptr(c.Uint64()))
		}
	}

	panic(fmt.Sprintf("constValue: %s", c))
}

// asInt converts x, which must be an integer, to an int suitable for
// use as a slice or array index or operand to make().
func asInt(x value) int {
	switch x := x.(type) {
	case int:
		return x
	case int8:
		return int(x)
	case int16:
		return int(x)
	case int32:
		return int(x)
	case int64:
		return int(x)
	case uint:
		return int(x)
	case uint8:
		return int(x)
	case uint16:
		return int(x)
	case uint32:
		return int(x)
	case uint64:
		return int(x)
	case uintptr:
		return int(x)
	default:
		v := reflect.ValueOf(x)
		switch v.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return int(v.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return int(v.Uint())
		}
	}
	panic(fmt.Sprintf("cannot convert %T to int", x))
}

// asUint64 converts x, which must be an unsigned integer, to a uint64
// suitable for use as a bitwise shift count.
func asUint64(x value) uint64 {
	switch x := x.(type) {
	case uint:
		return uint64(x)
	case uint8:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint32:
		return uint64(x)
	case uint64:
		return x
	case uintptr:
		return uint64(x)
	default:
		v := reflect.ValueOf(x)
		switch v.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			v.Uint()
		}
	}
	panic(fmt.Sprintf("cannot convert %T to uint64", x))
}

// slice returns x[lo:hi:max].  Any of lo, hi and max may be nil.
func slice(fr *frame, instr *ssa.Slice) value {
	_, makeslice := instr.X.(*ssa.Alloc)
	x := fr.get(instr.X)
	lo := fr.get(instr.Low)
	hi := fr.get(instr.High)
	max := fr.get(instr.Max)
	var Len, Cap int
	v := reflect.ValueOf(x)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.String:
		Len = v.Len()
		Cap = Len
	case reflect.Slice, reflect.Array:
		Len = v.Len()
		Cap = v.Cap()
	}

	l := 0
	if lo != nil {
		l = asInt(lo)
	}

	h := Len
	if hi != nil {
		h = asInt(hi)
	}

	var slice3 bool

	m := Cap
	if max != nil {
		m = asInt(max)
		slice3 = true
	}

	kind := v.Kind()
	if makeslice {
		if h < 0 {
			panic(runtimeError("makeslice: len out of range"))
		} else if h > m {
			panic(runtimeError("makeslice: cap out of range"))
		}
	} else {
		if slice3 {
			if m < 0 {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [::%v]", m)))
			} else if m > Cap {
				if kind == reflect.Slice {
					panic(runtimeError(fmt.Sprintf("slice bounds out of range [::%v] with capacity %v", m, Cap)))
				} else {
					panic(runtimeError(fmt.Sprintf("slice bounds out of range [::%v] with length %v", m, Cap)))
				}
			} else if h < 0 {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [:%v:]", h)))
			} else if h > m {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [:%v:%v]", h, m)))
			} else if l < 0 {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [%v::]", l)))
			} else if l > h {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [%v:%v:]", l, h)))
			}
		} else {
			if h < 0 {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [:%v]", h)))
			} else if h > Cap {
				if kind == reflect.Slice {
					panic(runtimeError(fmt.Sprintf("slice bounds out of range [:%v] with capacity %v", h, Cap)))
				} else {
					panic(runtimeError(fmt.Sprintf("slice bounds out of range [:%v] with length %v", h, Cap)))
				}
			} else if l < 0 {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [%v:]", l)))
			} else if l > h {
				panic(runtimeError(fmt.Sprintf("slice bounds out of range [%v:%v]", l, h)))
			}
		}
	}
	switch kind {
	case reflect.String:
		// optimization x[len(x):], see $GOROOT/test/slicecap.go
		if l == h {
			return v.Slice(0, 0).Interface()
		}
		return v.Slice(l, h).Interface()
	case reflect.Slice, reflect.Array:
		return v.Slice3(l, h, m).Interface()
	}
	panic(fmt.Sprintf("slice: unexpected X type: %T", x))
}

func opADD(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) + y.(int)
	case int8:
		return x.(int8) + y.(int8)
	case int16:
		return x.(int16) + y.(int16)
	case int32:
		return x.(int32) + y.(int32)
	case int64:
		return x.(int64) + y.(int64)
	case uint:
		return x.(uint) + y.(uint)
	case uint8:
		return x.(uint8) + y.(uint8)
	case uint16:
		return x.(uint16) + y.(uint16)
	case uint32:
		return x.(uint32) + y.(uint32)
	case uint64:
		return x.(uint64) + y.(uint64)
	case uintptr:
		return x.(uintptr) + y.(uintptr)
	case float32:
		return x.(float32) + y.(float32)
	case float64:
		return x.(float64) + y.(float64)
	case complex64:
		return x.(complex64) + y.(complex64)
	case complex128:
		return x.(complex128) + y.(complex128)
	case string:
		return x.(string) + y.(string)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() + vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() + vy.Uint())
			case reflect.Float32, reflect.Float64:
				r.SetFloat(vx.Float() + vy.Float())
			case reflect.Complex64, reflect.Complex128:
				r.SetComplex(vx.Complex() + vy.Complex())
			case reflect.String:
				r.SetString(vx.String() + vy.String())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T + %T", x, y))
}

func opSUB(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) - y.(int)
	case int8:
		return x.(int8) - y.(int8)
	case int16:
		return x.(int16) - y.(int16)
	case int32:
		return x.(int32) - y.(int32)
	case int64:
		return x.(int64) - y.(int64)
	case uint:
		return x.(uint) - y.(uint)
	case uint8:
		return x.(uint8) - y.(uint8)
	case uint16:
		return x.(uint16) - y.(uint16)
	case uint32:
		return x.(uint32) - y.(uint32)
	case uint64:
		return x.(uint64) - y.(uint64)
	case uintptr:
		return x.(uintptr) - y.(uintptr)
	case float32:
		return x.(float32) - y.(float32)
	case float64:
		return x.(float64) - y.(float64)
	case complex64:
		return x.(complex64) - y.(complex64)
	case complex128:
		return x.(complex128) - y.(complex128)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() - vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() - vy.Uint())
			case reflect.Float32, reflect.Float64:
				r.SetFloat(vx.Float() - vy.Float())
			case reflect.Complex64, reflect.Complex128:
				r.SetComplex(vx.Complex() - vy.Complex())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T - %T", x, y))
}

func opMUL(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) * y.(int)
	case int8:
		return x.(int8) * y.(int8)
	case int16:
		return x.(int16) * y.(int16)
	case int32:
		return x.(int32) * y.(int32)
	case int64:
		return x.(int64) * y.(int64)
	case uint:
		return x.(uint) * y.(uint)
	case uint8:
		return x.(uint8) * y.(uint8)
	case uint16:
		return x.(uint16) * y.(uint16)
	case uint32:
		return x.(uint32) * y.(uint32)
	case uint64:
		return x.(uint64) * y.(uint64)
	case uintptr:
		return x.(uintptr) * y.(uintptr)
	case float32:
		return x.(float32) * y.(float32)
	case float64:
		return x.(float64) * y.(float64)
	case complex64:
		return x.(complex64) * y.(complex64)
	case complex128:
		return x.(complex128) * y.(complex128)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() * vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() * vy.Uint())
			case reflect.Float32, reflect.Float64:
				r.SetFloat(vx.Float() * vy.Float())
			case reflect.Complex64, reflect.Complex128:
				r.SetComplex(vx.Complex() * vy.Complex())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T * %T", x, y))
}

func opQuo(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) / y.(int)
	case int8:
		return x.(int8) / y.(int8)
	case int16:
		return x.(int16) / y.(int16)
	case int32:
		return x.(int32) / y.(int32)
	case int64:
		return x.(int64) / y.(int64)
	case uint:
		return x.(uint) / y.(uint)
	case uint8:
		return x.(uint8) / y.(uint8)
	case uint16:
		return x.(uint16) / y.(uint16)
	case uint32:
		return x.(uint32) / y.(uint32)
	case uint64:
		return x.(uint64) / y.(uint64)
	case uintptr:
		return x.(uintptr) / y.(uintptr)
	case float32:
		return x.(float32) / y.(float32)
	case float64:
		return x.(float64) / y.(float64)
	case complex64:
		return x.(complex64) / y.(complex64)
	case complex128:
		return x.(complex128) / y.(complex128)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() / vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() / vy.Uint())
			case reflect.Float32, reflect.Float64:
				r.SetFloat(vx.Float() / vy.Float())
			case reflect.Complex64, reflect.Complex128:
				r.SetComplex(vx.Complex() / vy.Complex())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T / %T", x, y))
}

func opREM(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) % y.(int)
	case int8:
		return x.(int8) % y.(int8)
	case int16:
		return x.(int16) % y.(int16)
	case int32:
		return x.(int32) % y.(int32)
	case int64:
		return x.(int64) % y.(int64)
	case uint:
		return x.(uint) % y.(uint)
	case uint8:
		return x.(uint8) % y.(uint8)
	case uint16:
		return x.(uint16) % y.(uint16)
	case uint32:
		return x.(uint32) % y.(uint32)
	case uint64:
		return x.(uint64) % y.(uint64)
	case uintptr:
		return x.(uintptr) % y.(uintptr)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() % vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() % vy.Uint())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T %% %T", x, y))
}

func opAND(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) & y.(int)
	case int8:
		return x.(int8) & y.(int8)
	case int16:
		return x.(int16) & y.(int16)
	case int32:
		return x.(int32) & y.(int32)
	case int64:
		return x.(int64) & y.(int64)
	case uint:
		return x.(uint) & y.(uint)
	case uint8:
		return x.(uint8) & y.(uint8)
	case uint16:
		return x.(uint16) & y.(uint16)
	case uint32:
		return x.(uint32) & y.(uint32)
	case uint64:
		return x.(uint64) & y.(uint64)
	case uintptr:
		return x.(uintptr) & y.(uintptr)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() & vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() & vy.Uint())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T && %T", x, y))
}

func opOR(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) | y.(int)
	case int8:
		return x.(int8) | y.(int8)
	case int16:
		return x.(int16) | y.(int16)
	case int32:
		return x.(int32) | y.(int32)
	case int64:
		return x.(int64) | y.(int64)
	case uint:
		return x.(uint) | y.(uint)
	case uint8:
		return x.(uint8) | y.(uint8)
	case uint16:
		return x.(uint16) | y.(uint16)
	case uint32:
		return x.(uint32) | y.(uint32)
	case uint64:
		return x.(uint64) | y.(uint64)
	case uintptr:
		return x.(uintptr) | y.(uintptr)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() | vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() | vy.Uint())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T | %T", x, y))
}

func opXOR(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) ^ y.(int)
	case int8:
		return x.(int8) ^ y.(int8)
	case int16:
		return x.(int16) ^ y.(int16)
	case int32:
		return x.(int32) ^ y.(int32)
	case int64:
		return x.(int64) ^ y.(int64)
	case uint:
		return x.(uint) ^ y.(uint)
	case uint8:
		return x.(uint8) ^ y.(uint8)
	case uint16:
		return x.(uint16) ^ y.(uint16)
	case uint32:
		return x.(uint32) ^ y.(uint32)
	case uint64:
		return x.(uint64) ^ y.(uint64)
	case uintptr:
		return x.(uintptr) ^ y.(uintptr)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() ^ vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() ^ vy.Uint())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T ^ %T", x, y))
}

func opANDNOT(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) &^ y.(int)
	case int8:
		return x.(int8) &^ y.(int8)
	case int16:
		return x.(int16) &^ y.(int16)
	case int32:
		return x.(int32) &^ y.(int32)
	case int64:
		return x.(int64) &^ y.(int64)
	case uint:
		return x.(uint) &^ y.(uint)
	case uint8:
		return x.(uint8) &^ y.(uint8)
	case uint16:
		return x.(uint16) &^ y.(uint16)
	case uint32:
		return x.(uint32) &^ y.(uint32)
	case uint64:
		return x.(uint64) &^ y.(uint64)
	case uintptr:
		return x.(uintptr) &^ y.(uintptr)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			r := reflect.New(vx.Type()).Elem()
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(vx.Int() &^ vy.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(vx.Uint() &^ vy.Uint())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T &^ %T", x, y))
}

func opSHL(x, _y value) value {
	y := asUint64(_y)
	switch x.(type) {
	case int:
		return x.(int) << y
	case int8:
		return x.(int8) << y
	case int16:
		return x.(int16) << y
	case int32:
		return x.(int32) << y
	case int64:
		return x.(int64) << y
	case uint:
		return x.(uint) << y
	case uint8:
		return x.(uint8) << y
	case uint16:
		return x.(uint16) << y
	case uint32:
		return x.(uint32) << y
	case uint64:
		return x.(uint64) << y
	case uintptr:
		return x.(uintptr) << y
	default:
		vx := reflect.ValueOf(x)
		r := reflect.New(vx.Type()).Elem()
		switch vx.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			r.SetInt(vx.Int() << y)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			r.SetUint(vx.Uint() << y)
		default:
			goto failed
		}
		return r.Interface()
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T << %T", x, y))
}

func opSHR(x, _y value) value {
	y := asUint64(_y)
	switch x.(type) {
	case int:
		return x.(int) >> y
	case int8:
		return x.(int8) >> y
	case int16:
		return x.(int16) >> y
	case int32:
		return x.(int32) >> y
	case int64:
		return x.(int64) >> y
	case uint:
		return x.(uint) >> y
	case uint8:
		return x.(uint8) >> y
	case uint16:
		return x.(uint16) >> y
	case uint32:
		return x.(uint32) >> y
	case uint64:
		return x.(uint64) >> y
	case uintptr:
		return x.(uintptr) >> y
	default:
		vx := reflect.ValueOf(x)
		r := reflect.New(vx.Type()).Elem()
		switch vx.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			r.SetInt(vx.Int() >> y)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			r.SetUint(vx.Uint() >> y)
		default:
			goto failed
		}
		return r.Interface()
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T >> %T", x, y))
}

func opLSS(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) < y.(int)
	case int8:
		return x.(int8) < y.(int8)
	case int16:
		return x.(int16) < y.(int16)
	case int32:
		return x.(int32) < y.(int32)
	case int64:
		return x.(int64) < y.(int64)
	case uint:
		return x.(uint) < y.(uint)
	case uint8:
		return x.(uint8) < y.(uint8)
	case uint16:
		return x.(uint16) < y.(uint16)
	case uint32:
		return x.(uint32) < y.(uint32)
	case uint64:
		return x.(uint64) < y.(uint64)
	case uintptr:
		return x.(uintptr) < y.(uintptr)
	case float32:
		return x.(float32) < y.(float32)
	case float64:
		return x.(float64) < y.(float64)
	case string:
		return x.(string) < y.(string)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				return vx.Int() < vy.Int()
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				return vx.Uint() < vy.Uint()
			case reflect.Float32, reflect.Float64:
				return vx.Float() < vy.Float()
			case reflect.String:
				return vx.String() < vy.String()
			default:
				goto failed
			}
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T < %T", x, y))
}

func opLEQ(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) <= y.(int)
	case int8:
		return x.(int8) <= y.(int8)
	case int16:
		return x.(int16) <= y.(int16)
	case int32:
		return x.(int32) <= y.(int32)
	case int64:
		return x.(int64) <= y.(int64)
	case uint:
		return x.(uint) <= y.(uint)
	case uint8:
		return x.(uint8) <= y.(uint8)
	case uint16:
		return x.(uint16) <= y.(uint16)
	case uint32:
		return x.(uint32) <= y.(uint32)
	case uint64:
		return x.(uint64) <= y.(uint64)
	case uintptr:
		return x.(uintptr) <= y.(uintptr)
	case float32:
		return x.(float32) <= y.(float32)
	case float64:
		return x.(float64) <= y.(float64)
	case string:
		return x.(string) <= y.(string)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				return vx.Int() <= vy.Int()
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				return vx.Uint() <= vy.Uint()
			case reflect.Float32, reflect.Float64:
				return vx.Float() <= vy.Float()
			case reflect.String:
				return vx.String() <= vy.String()
			default:
				goto failed
			}
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T <= %T", x, y))
}

func opGTR(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) > y.(int)
	case int8:
		return x.(int8) > y.(int8)
	case int16:
		return x.(int16) > y.(int16)
	case int32:
		return x.(int32) > y.(int32)
	case int64:
		return x.(int64) > y.(int64)
	case uint:
		return x.(uint) > y.(uint)
	case uint8:
		return x.(uint8) > y.(uint8)
	case uint16:
		return x.(uint16) > y.(uint16)
	case uint32:
		return x.(uint32) > y.(uint32)
	case uint64:
		return x.(uint64) > y.(uint64)
	case uintptr:
		return x.(uintptr) > y.(uintptr)
	case float32:
		return x.(float32) > y.(float32)
	case float64:
		return x.(float64) > y.(float64)
	case string:
		return x.(string) > y.(string)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				return vx.Int() > vy.Int()
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				return vx.Uint() > vy.Uint()
			case reflect.Float32, reflect.Float64:
				return vx.Float() > vy.Float()
			case reflect.String:
				return vx.String() > vy.String()
			default:
				goto failed
			}
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T > %T", x, y))
}

func opGEQ(x, y value) value {
	switch x.(type) {
	case int:
		return x.(int) >= y.(int)
	case int8:
		return x.(int8) >= y.(int8)
	case int16:
		return x.(int16) >= y.(int16)
	case int32:
		return x.(int32) >= y.(int32)
	case int64:
		return x.(int64) >= y.(int64)
	case uint:
		return x.(uint) >= y.(uint)
	case uint8:
		return x.(uint8) >= y.(uint8)
	case uint16:
		return x.(uint16) >= y.(uint16)
	case uint32:
		return x.(uint32) >= y.(uint32)
	case uint64:
		return x.(uint64) >= y.(uint64)
	case uintptr:
		return x.(uintptr) >= y.(uintptr)
	case float32:
		return x.(float32) >= y.(float32)
	case float64:
		return x.(float64) >= y.(float64)
	case string:
		return x.(string) >= y.(string)
	default:
		vx := reflect.ValueOf(x)
		vy := reflect.ValueOf(y)
		if kind := vx.Kind(); kind == vy.Kind() {
			switch kind {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				return vx.Int() >= vy.Int()
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				return vx.Uint() >= vy.Uint()
			case reflect.Float32, reflect.Float64:
				return vx.Float() >= vy.Float()
			case reflect.String:
				return vx.String() >= vy.String()
			default:
				goto failed
			}
		}
	}
failed:
	panic(fmt.Sprintf("invalid binary op: %T >= %T", x, y))
}

// binop implements all arithmetic and logical binary operators for
// numeric datatypes and strings.  Both operands must have identical
// dynamic type.
//
func binop(instr *ssa.BinOp, t types.Type, x, y value) value {
	switch instr.Op {
	case token.ADD:
		return opADD(x, y)
	case token.SUB:
		return opSUB(x, y)
	case token.MUL:
		return opMUL(x, y)
	case token.QUO:
		return opQuo(x, y)
	case token.REM:
		return opREM(x, y)
	case token.AND:
		return opAND(x, y)
	case token.OR:
		return opOR(x, y)
	case token.XOR:
		return opXOR(x, y)
	case token.AND_NOT:
		return opANDNOT(x, y)
	case token.SHL:
		return opSHL(x, y)
	case token.SHR:
		return opSHR(x, y)
	case token.LSS:
		return opLSS(x, y)
	case token.LEQ:
		return opLEQ(x, y)
	case token.EQL:
		return opEQL(instr, x, y)
	case token.NEQ:
		return !opEQL(instr, x, y)
	case token.GTR:
		return opGTR(x, y)
	case token.GEQ:
		return opGEQ(x, y)
	}
	panic(fmt.Sprintf("invalid binary op: %T %s %T", x, instr.Op, y))
}

func IsConstNil(v ssa.Value) bool {
	if c, ok := v.(*ssa.Const); ok {
		return c.IsNil()
	}
	return false
}

func IsNil(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Invalid:
		return true
	case reflect.Slice, reflect.Map, reflect.Func:
		return v.IsNil()
	case reflect.Chan, reflect.Ptr, reflect.UnsafePointer, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

func opEQL(instr *ssa.BinOp, x, y interface{}) bool {
	vx := reflect.ValueOf(x)
	vy := reflect.ValueOf(y)
	if vx.Kind() != vy.Kind() {
		return false
	}
	if IsConstNil(instr.X) {
		return IsNil(vy)
	} else if IsConstNil(instr.Y) {
		return IsNil(vx)
	}
	return equalValue(vx, vy)
}

func equalNil(vx, vy reflect.Value) bool {
	if IsNil(vx) {
		return IsNil(vy)
	} else if IsNil(vy) {
		return IsNil(vx)
	}
	return equalValue(vx, vy)
}

func equalValue(vx, vy reflect.Value) bool {
	if kind := vx.Kind(); kind == vy.Kind() {
		switch kind {
		case reflect.Invalid:
			return true
		case reflect.Chan:
			dirx := vx.Type().ChanDir()
			diry := vy.Type().ChanDir()
			if dirx != diry {
				if dirx == reflect.BothDir {
					return vy.Interface() == vx.Convert(vy.Type()).Interface()
				} else if diry == reflect.BothDir {
					return vx.Interface() == vy.Convert(vx.Type()).Interface()
				}
			} else {
				return vx.Interface() == vy.Interface()
			}
		case reflect.Ptr:
			return vx.Pointer() == vy.Pointer()
		case reflect.Struct:
			return equalStruct(vx, vy)
		case reflect.Array:
			return equalArray(vx, vy)
		default:
			return vx.Interface() == vy.Interface()
		}
	}
	return false
}

func equalArray(vx, vy reflect.Value) bool {
	xlen := vx.Len()
	if xlen != vy.Len() {
		return false
	}
	if vx.Type().Elem() != vy.Type().Elem() {
		return false
	}
	for i := 0; i < xlen; i++ {
		fx := vx.Index(i)
		fy := vy.Index(i)
		if !equalNil(fx, fy) {
			return false
		}
	}
	return true
}

func equalStruct(vx, vy reflect.Value) bool {
	typ := vx.Type()
	if typ != vy.Type() {
		return false
	}
	n := typ.NumField()
	for i := 0; i < n; i++ {
		f := typ.Field(i)
		if f.Name == "_" {
			continue
		}
		fx := reflectx.FieldByIndexX(vx, f.Index)
		fy := reflectx.FieldByIndexX(vy, f.Index)
		// check uncomparable
		switch f.Type.Kind() {
		case reflect.Slice, reflect.Map, reflect.Func:
			if fx.Interface() != fy.Interface() {
				return false
			}
		}
		if !equalNil(fx, fy) {
			return false
		}
	}
	return true
}

func unop(instr *ssa.UnOp, x value) value {
	switch instr.Op {
	case token.ARROW: // receive
		vx := reflect.ValueOf(x)
		v, ok := vx.Recv()
		if !ok {
			v = reflect.New(vx.Type().Elem()).Elem()
		}
		if instr.CommaOk {
			return tuple{v.Interface(), ok}
		}
		return v.Interface()
		// if !ok {
		// 	v = zero(instr.X.Type().Underlying().(*types.Chan).Elem())
		// }
		// if instr.CommaOk {
		// 	v = tuple{v, ok}
		// }
		// return v
	case token.SUB:
		switch x := x.(type) {
		case int:
			return -x
		case int8:
			return -x
		case int16:
			return -x
		case int32:
			return -x
		case int64:
			return -x
		case uint:
			return -x
		case uint8:
			return -x
		case uint16:
			return -x
		case uint32:
			return -x
		case uint64:
			return -x
		case uintptr:
			return -x
		case float32:
			return -x
		case float64:
			return -x
		case complex64:
			return -x
		case complex128:
			return -x
		default:
			v := reflect.ValueOf(x)
			r := reflect.New(v.Type()).Elem()
			switch v.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(-v.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(-v.Uint())
			case reflect.Float32, reflect.Float64:
				r.SetFloat(-v.Float())
			case reflect.Complex64, reflect.Complex128:
				r.SetComplex(-v.Complex())
			}
			return r.Interface()
		}
	case token.MUL:
		v := reflect.ValueOf(x).Elem()
		if !v.IsValid() {
			panic(runtimeError("invalid memory address or nil pointer dereference"))
		}
		return v.Interface()
		//return load(deref(instr.X.Type()), x.(*value))
	case token.NOT:
		switch x := x.(type) {
		case bool:
			return !x
		default:
			v := reflect.ValueOf(x)
			if v.Kind() == reflect.Bool {
				r := reflect.New(v.Type()).Elem()
				if v.Bool() {
					return v.Interface()
				}
				r.SetBool(true)
				return r.Interface()
			}
		}
		// return !x.(bool)
	case token.XOR:
		switch x := x.(type) {
		case int:
			return ^x
		case int8:
			return ^x
		case int16:
			return ^x
		case int32:
			return ^x
		case int64:
			return ^x
		case uint:
			return ^x
		case uint8:
			return ^x
		case uint16:
			return ^x
		case uint32:
			return ^x
		case uint64:
			return ^x
		case uintptr:
			return ^x
		default:
			vx := reflect.ValueOf(x)
			r := reflect.New(vx.Type()).Elem()
			switch vx.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				r.SetInt(^r.Int())
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				r.SetUint(^r.Uint())
			default:
				goto failed
			}
			return r.Interface()
		}
	}
failed:
	panic(fmt.Sprintf("invalid unary op %s %T", instr.Op, x))
}

// typeAssert checks whether dynamic type of itf is instr.AssertedType.
// It returns the extracted value on success, and panics on failure,
// unless instr.CommaOk, in which case it always returns a "value,ok" tuple.
//
func typeAssert(i *Interp, instr *ssa.TypeAssert, iv interface{}) value {
	var v value
	var err error
	typ := i.toType(instr.AssertedType)
	if iv == nil {
		err = plainError(fmt.Sprintf("interface conversion: interface is nil, not %v", typ))
	} else {
		rv := reflect.ValueOf(iv)
		rt := rv.Type()
		if typ == rt {
			v = iv
		} else {
			if !rt.AssignableTo(typ) {
				err = runtimeError(fmt.Sprintf("interface conversion: %v is %v, not %v", instr.X.Type(), rt, typ))
				if itype, ok := instr.AssertedType.Underlying().(*types.Interface); ok {
					if it, ok := i.findType(rt, false); ok {
						if meth, _ := types.MissingMethod(it, itype, true); meth != nil {
							err = runtimeError(fmt.Sprintf("interface conversion: %v is not %v: missing method %s",
								rt, instr.AssertedType, meth.Name()))
						}
					}
				} else if typ.PkgPath() == rt.PkgPath() && typ.Name() == rt.Name() {
					t1, ok1 := i.findType(typ, false)
					t2, ok2 := i.findType(rt, false)
					if ok1 && ok2 {
						n1, ok1 := t1.(*types.Named)
						n2, ok2 := t2.(*types.Named)
						if ok1 && ok2 && n1.Obj().Parent() != n2.Obj().Parent() {
							err = runtimeError(fmt.Sprintf("interface conversion: %v is %v, not %v (types from different scopes)", instr.X.Type(), rt, typ))
						}
					}
				}
			} else {
				v = rv.Convert(typ).Interface()
			}
		}
	}
	if err != nil {
		if !instr.CommaOk {
			panic(err)
		}
		return tuple{reflect.New(typ).Elem().Interface(), false}
	}
	if instr.CommaOk {
		return tuple{v, true}
	}
	return v
	// err := ""
	// if itf.t == nil {
	// 	err = fmt.Sprintf("interface conversion: interface is nil, not %s", instr.AssertedType)

	// } else if idst, ok := instr.AssertedType.Underlying().(*types.Interface); ok {
	// 	v = itf
	// 	err = checkInterface(i, idst, itf)

	// } else if types.Identical(itf.t, instr.AssertedType) {
	// 	v = itf.v // extract value

	// } else {
	// 	err = fmt.Sprintf("interface conversion: interface is %s, not %s", itf.t, instr.AssertedType)
	// }

	// if err != "" {
	// 	if !instr.CommaOk {
	// 		panic(err)
	// 	}
	// 	return tuple{zero(instr.AssertedType), false}
	// }
	// if instr.CommaOk {
	// 	return tuple{v, true}
	// }
	// return v
}

// If CapturedOutput is non-nil, all writes by the interpreted program
// to file descriptors 1 and 2 will also be written to CapturedOutput.
//
// (The $GOROOT/test system requires that the test be considered a
// failure if "BUG" appears in the combined stdout/stderr output, even
// if it exits zero.  This is a global variable shared by all
// interpreters in the same process.)
//
var CapturedOutput *bytes.Buffer
var capturedOutputMu sync.Mutex

// write writes bytes b to the target program's standard output.
// The print/println built-ins and the write() system call funnel
// through here so they can be captured by the test driver.
func print(b []byte) (int, error) {
	if CapturedOutput != nil {
		capturedOutputMu.Lock()
		CapturedOutput.Write(b) // ignore errors
		capturedOutputMu.Unlock()
	}
	return os.Stdout.Write(b)
}

// callBuiltin interprets a call to builtin fn with arguments args,
// returning its result.
func callBuiltin(inter *Interp, caller *frame, callpos token.Pos, fn *ssa.Builtin, args []value, ssaArgs []ssa.Value) value {
	switch fn.Name() {
	case "append":
		if len(args) == 1 {
			return args[0]
		}
		if s, ok := args[1].(string); ok {
			// append([]byte, ...string) []byte
			args[1] = []byte(s)
		}
		v0 := reflect.ValueOf(args[0])
		v1 := reflect.ValueOf(args[1])
		i0 := v0.Len()
		i1 := v1.Len()
		if i0+i1 < i0 {
			panic(runtimeError("growslice: cap out of range"))
		}
		return reflect.AppendSlice(v0, v1).Interface()
		// append([]T, ...[]T) []T
		// return append(args[0].([]value), args[1].([]value)...)

	case "copy": // copy([]T, []T) int or copy([]byte, string) int
		return reflect.Copy(reflect.ValueOf(args[0]), reflect.ValueOf(args[1]))
		// src := args[1]
		// if _, ok := src.(string); ok {
		// 	params := fn.Type().(*types.Signature).Params()
		// 	src = conv(params.At(0).Type(), params.At(1).Type(), src)
		// }
		// return copy(args[0].([]value), src.([]value))

	case "close": // close(chan T)
		//close(args[0].(chan value))
		reflect.ValueOf(args[0]).Close()
		return nil

	case "delete": // delete(map[K]value, K)
		reflect.ValueOf(args[0]).SetMapIndex(reflect.ValueOf(args[1]), reflect.Value{})
		// switch m := args[0].(type) {
		// case map[value]value:
		// 	delete(m, args[1])
		// case *hashmap:
		// 	m.delete(args[1].(hashable))
		// default:
		// 	panic(fmt.Sprintf("illegal map type: %T", m))
		// }
		return nil

	case "print", "println": // print(any, ...)
		ln := fn.Name() == "println"
		var buf bytes.Buffer
		for i, arg := range args {
			if i > 0 && ln {
				buf.WriteRune(' ')
			}
			if len(ssaArgs) > i {
				typ := inter.toType(ssaArgs[i].Type())
				if typ.Kind() == reflect.Interface {
					buf.WriteString(toInterface(arg))
					continue
				}
			}
			buf.WriteString(toString(arg))
		}
		if ln {
			buf.WriteRune('\n')
		}
		print(buf.Bytes())
		return nil

	case "len":
		return reflect.ValueOf(args[0]).Len()
		// switch x := args[0].(type) {
		// case string:
		// 	return len(x)
		// case array:
		// 	return len(x)
		// case *value:
		// 	return len((*x).(array))
		// case []value:
		// 	return len(x)
		// case map[value]value:
		// 	return len(x)
		// case *hashmap:
		// 	return x.len()
		// case chan value:
		// 	return len(x)
		// default:
		// 	panic(fmt.Sprintf("len: illegal operand: %T", x))
		// }

	case "cap":
		return reflect.ValueOf(args[0]).Cap()
		// switch x := args[0].(type) {
		// case array:
		// 	return cap(x)
		// case *value:
		// 	return cap((*x).(array))
		// case []value:
		// 	return cap(x)
		// case chan value:
		// 	return cap(x)
		// default:
		// 	panic(fmt.Sprintf("cap: illegal operand: %T", x))
		// }

	case "real":
		c := reflect.ValueOf(args[0])
		switch c.Kind() {
		case reflect.Complex64:
			return real(complex64(c.Complex()))
		case reflect.Complex128:
			return real(c.Complex())
		default:
			panic(fmt.Sprintf("real: illegal operand: %T", c))
		}

	case "imag":
		c := reflect.ValueOf(args[0])
		switch c.Kind() {
		case reflect.Complex64:
			return imag(complex64(c.Complex()))
		case reflect.Complex128:
			return imag(c.Complex())
		default:
			panic(fmt.Sprintf("imag: illegal operand: %T", c))
		}

	case "complex":
		r := reflect.ValueOf(args[0])
		i := reflect.ValueOf(args[1])
		switch r.Kind() {
		case reflect.Float32:
			return complex(float32(r.Float()), float32(i.Float()))
		case reflect.Float64:
			return complex(r.Float(), i.Float())
		default:
			panic(fmt.Sprintf("complex: illegal operand: %v", r.Kind()))
		}

	case "panic":
		// ssa.Panic handles most cases; this is only for "go
		// panic" or "defer panic".
		panic(targetPanic{args[0]})

	case "recover":
		return doRecover(caller)

	case "ssa:wrapnilchk":
		recv := args[0]
		if reflect.ValueOf(recv).IsNil() {
			recvType := args[1]
			methodName := args[2]
			var info value
			if s, ok := recvType.(string); ok && strings.HasPrefix(s, "main.") {
				info = s[5:]
			} else {
				info = recvType
			}
			panic(plainError(fmt.Sprintf("value method %s.%s called using nil *%s pointer",
				recvType, methodName, info)))
		}
		return recv

	case "Add":
		ptr := args[0].(unsafe.Pointer)
		length := asInt(args[1])
		return unsafe.Pointer(uintptr(ptr) + uintptr(length))
	case "Slice":
		//func Slice(ptr *ArbitraryType, len IntegerType) []ArbitraryType
		//(*[len]ArbitraryType)(unsafe.Pointer(ptr))[:]
		ptr := reflect.ValueOf(args[0])
		length := asInt(args[1])
		if ptr.IsNil() {
			if length == 0 {
				return reflect.New(reflect.SliceOf(ptr.Type().Elem())).Elem().Interface()
			}
			panic(runtimeError("unsafe.Slice: ptr is nil and len is not zero"))
		}
		typ := reflect.ArrayOf(length, ptr.Type().Elem())
		v := reflect.NewAt(typ, unsafe.Pointer(ptr.Pointer()))
		return v.Elem().Slice(0, length).Interface()
	}

	panic("unknown built-in: " + fn.Name())
}

func rangeIter(x value, t types.Type) iter {
	switch x := x.(type) {
	case string:
		return &stringIter{Reader: strings.NewReader(x)}
	default:
		return &mapIter{iter: reflect.ValueOf(x).MapRange()}
	}
	// switch x := x.(type) {
	// case map[value]value:
	// 	return &mapIter{iter: reflect.ValueOf(x).MapRange()}
	// case *hashmap:
	// 	return &hashmapIter{iter: reflect.ValueOf(x.entries()).MapRange()}
	// case string:
	// 	return &stringIter{Reader: strings.NewReader(x)}
	// }
	// panic(fmt.Sprintf("cannot range over %T", x))
}

// widen widens a basic typed value x to the widest type of its
// category, one of:
//   bool, int64, uint64, float64, complex128, string.
// This is inefficient but reduces the size of the cross-product of
// cases we have to consider.
//
func widen(x value) value {
	switch y := x.(type) {
	case bool, int64, uint64, float64, complex128, string, unsafe.Pointer:
		return x
	case int:
		return int64(y)
	case int8:
		return int64(y)
	case int16:
		return int64(y)
	case int32:
		return int64(y)
	case uint:
		return uint64(y)
	case uint8:
		return uint64(y)
	case uint16:
		return uint64(y)
	case uint32:
		return uint64(y)
	case uintptr:
		return uint64(y)
	case float32:
		return float64(y)
	case complex64:
		return complex128(y)
	}
	panic(fmt.Sprintf("cannot widen %T", x))
}

//go:nocheckptr
func toUnsafePointer(v reflect.Value) unsafe.Pointer {
	return unsafe.Pointer(uintptr(v.Uint()))
}

func convert(x interface{}, typ reflect.Type) interface{} {
	v := reflect.ValueOf(x)
	vk := v.Kind()
	switch typ.Kind() {
	case reflect.UnsafePointer:
		if vk == reflect.Uintptr {
			return toUnsafePointer(v)
		} else if vk == reflect.Ptr {
			return unsafe.Pointer(v.Pointer())
		}
	case reflect.Uintptr:
		if vk == reflect.UnsafePointer {
			return v.Pointer()
		}
	case reflect.Ptr:
		if vk == reflect.UnsafePointer {
			return reflect.NewAt(typ.Elem(), unsafe.Pointer(v.Pointer())).Interface()
		}
	}
	return v.Convert(typ).Interface()
}

// checkInterface checks that the method set of x implements the
// interface itype.
// On success it returns "", on failure, an error message.
//
func checkInterface(i *Interp, itype *types.Interface, x iface) string {
	if meth, _ := types.MissingMethod(x.t, itype, true); meth != nil {
		return fmt.Sprintf("interface conversion: %v is not %v: missing method %s",
			x.t, itype, meth.Name())
	}
	return "" // ok
}
