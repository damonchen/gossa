// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package ssa/interp defines an interpreter for the SSA
// representation of Go programs.
//
// This interpreter is provided as an adjunct for testing the SSA
// construction algorithm.  Its purpose is to provide a minimal
// metacircular implementation of the dynamic semantics of each SSA
// instruction.  It is not, and will never be, a production-quality Go
// interpreter.
//
// The following is a partial list of Go features that are currently
// unsupported or incomplete in the interpreter.
//
// * Unsafe operations, including all uses of unsafe.Pointer, are
// impossible to support given the "boxed" value representation we
// have chosen.
//
// * The reflect package is only partially implemented.
//
// * The "testing" package is no longer supported because it
// depends on low-level details that change too often.
//
// * "sync/atomic" operations are not atomic due to the "boxed" value
// representation: it is not possible to read, modify and write an
// interface value atomically. As a consequence, Mutexes are currently
// broken.
//
// * recover is only partially implemented.  Also, the interpreter
// makes no attempt to distinguish target panics from interpreter
// crashes.
//
// * the sizes of the int, uint and uintptr types in the target
// program are assumed to be the same as those of the interpreter
// itself.
//
// * all values occupy space, even those of types defined by the spec
// to have zero size, e.g. struct{}.  This can cause asymptotic
// performance degradation.
//
// * os.Exit is implemented using panic, causing deferred functions to
// run.
package gossa

import (
	"fmt"
	"go/constant"
	"go/token"
	"go/types"
	"log"
	"reflect"
	"runtime"
	"sync"
	"unsafe"

	"github.com/goplus/reflectx"
	"golang.org/x/tools/go/ssa"
)

var (
	maxMemLen int
)

const intSize = 32 << (^uint(0) >> 63)

func init() {
	if intSize == 32 {
		maxMemLen = 1<<31 - 1
	} else {
		v := int64(1) << 59
		maxMemLen = int(v)
	}
}

type continuation = int

const (
	kNext continuation = iota
	kReturn
	kJump
)

type plainError string

func (e plainError) Error() string {
	return string(e)
}

type runtimeError string

func (e runtimeError) RuntimeError() {}

func (e runtimeError) Error() string {
	return "runtime error: " + string(e)
}

// State shared between all interpreted goroutines.
type Interp struct {
	fset         *token.FileSet
	prog         *ssa.Program        // the SSA program
	mainpkg      *ssa.Package        // the SSA main package
	globals      map[ssa.Value]value // addresses of global variables (immutable)
	mode         Mode                // interpreter options
	sizes        types.Sizes         // the effective type-sizing function
	goroutines   int32               // atomically updated
	preloadTypes map[types.Type]reflect.Type
	caller       *frame
	loader       Loader
	record       *TypesRecord
	typesMutex   sync.RWMutex
	fnDebug      func(*DebugInfo)
	funcs        map[*ssa.Function]*Function
}

func (i *Interp) setDebug(fn func(*DebugInfo)) {
	i.fnDebug = fn
}

func (i *Interp) installed(path string) (pkg *Package, ok bool) {
	pkg, ok = i.loader.Installed(path)
	return
}

func (i *Interp) findType(rt reflect.Type, local bool) (types.Type, bool) {
	i.typesMutex.Lock()
	defer i.typesMutex.Unlock()
	if local {
		return i.record.LookupLocalTypes(rt)
	} else {
		return i.record.LookupTypes(rt)
	}
}

func (i *Interp) FindMethod(mtyp reflect.Type, fn *types.Func) func([]reflect.Value) []reflect.Value {
	typ := fn.Type().(*types.Signature).Recv().Type()
	if f := i.prog.LookupMethod(typ, fn.Pkg(), fn.Name()); f != nil {
		return func(args []reflect.Value) []reflect.Value {
			iargs := make([]value, len(args))
			for i := 0; i < len(args); i++ {
				iargs[i] = args[i].Interface()
			}
			r := i.call(nil, token.NoPos, f, iargs, nil)
			switch mtyp.NumOut() {
			case 0:
				return nil
			case 1:
				if r == nil {
					return []reflect.Value{reflect.New(mtyp.Out(0)).Elem()}
				} else {
					return []reflect.Value{reflect.ValueOf(r)}
				}
			default:
				v, ok := r.(tuple)
				if !ok {
					panic(fmt.Errorf("result type must tuple: %T", v))
				}
				res := make([]reflect.Value, len(v))
				for j := 0; j < len(v); j++ {
					if v[j] == nil {
						res[j] = reflect.New(mtyp.Out(j)).Elem()
					} else {
						res[j] = reflect.ValueOf(v[j])
					}
				}
				return res
			}
		}
	}
	name := fn.FullName()
	//	pkgPath := fn.Pkg().Path()
	if v, ok := externValues[name]; ok && v.Kind() == reflect.Func {
		return func(args []reflect.Value) []reflect.Value {
			return v.Call(args)
		}
	}
	// if pkg, ok := i.installed(pkgPath); ok {
	// 	if ext, ok := pkg.Methods[name]; ok {
	// 		return func(args []reflect.Value) []reflect.Value {
	// 			return ext.Call(args)
	// 		}
	// 	}
	// }
	panic(fmt.Sprintf("Not found func %v", fn))
	return nil
}

func (i *Interp) makeFunc(fr *frame, typ reflect.Type, f *ssa.Function) reflect.Value {
	return i.makeFuncEx(fr, typ, f, nil)
}

func (i *Interp) makeFuncEx(fr *frame, typ reflect.Type, fn *ssa.Function, env []value) reflect.Value {
	return reflect.MakeFunc(typ, func(args []reflect.Value) []reflect.Value {
		iargs := make([]value, len(args))
		for i := 0; i < len(args); i++ {
			iargs[i] = args[i].Interface()
		}
		r := i.callFunction(i.caller, token.NoPos, fn, iargs, env)
		if v, ok := r.(tuple); ok {
			res := make([]reflect.Value, len(v))
			for i := 0; i < len(v); i++ {
				if v[i] == nil {
					res[i] = reflect.New(typ.Out(i)).Elem()
				} else {
					res[i] = reflect.ValueOf(v[i])
				}
			}
			return res
		} else if typ.NumOut() == 1 {
			if r != nil {
				return []reflect.Value{reflect.ValueOf(r)}
			} else {
				return []reflect.Value{reflect.New(typ.Out(0)).Elem()}
			}
		}
		return nil
	})
}

type deferred struct {
	fn      value
	args    []value
	ssaArgs []ssa.Value
	instr   *ssa.Defer
	tail    *deferred
}

type frame struct {
	i                *Interp
	caller           *frame
	pfn              *Function
	block, prevBlock *FuncBlock
	env              map[ssa.Value]value // dynamic values of SSA variables
	locals           map[ssa.Value]reflect.Value
	mapUnderscoreKey map[types.Type]bool
	stack            []value
	defers           *deferred
	result           value
	panicking        bool
	panic            interface{}
}

func (r *frame) reg(i int) value {
	return r.stack[i]
}

func (r *frame) setReg(i int, v value) {
	r.stack[i] = v
}

func (fr *frame) get(key ssa.Value) value {
	if key == nil {
		return nil
	}
	switch key := key.(type) {
	case *ssa.Function:
		return key
	case *ssa.Builtin:
		return key
	case *constValue:
		return key.Value
	case *ssa.Const:
		return constToValue(fr.i, key)
	case *ssa.Global:
		if key.Pkg != nil {
			pkgpath := key.Pkg.Pkg.Path()
			if pkg, ok := fr.i.installed(pkgpath); ok {
				if ext, ok := pkg.Vars[key.Name()]; ok {
					return ext.Interface()
				}
			}
		}
		if r, ok := fr.i.globals[key]; ok {
			return r
		}
	}
	if key.Parent() == nil {
		path := key.String()
		if ext, ok := externValues[path]; ok {
			if fr.i.mode&EnableTracing != 0 {
				log.Println("\t(external)")
			}
			return ext.Interface()
		}
	}
	if r, ok := fr.env[key]; ok {
		return r
	}
	panic(fmt.Sprintf("get: no value for %T: %v", key, key.String()))
}

// runDefer runs a deferred call d.
// It always returns normally, but may set or clear fr.panic.
//
func (fr *frame) runDefer(d *deferred) {
	if fr.i.mode&EnableTracing != 0 {
		log.Printf("%s: invoking deferred function call\n",
			fr.i.prog.Fset.Position(d.instr.Pos()))
	}
	var ok bool
	defer func() {
		if !ok {
			// Deferred call created a new state of panic.
			fr.panicking = true
			fr.panic = recover()
		}
	}()
	fr.i.call(fr, d.instr.Pos(), d.fn, d.args, d.ssaArgs)
	ok = true
}

// runDefers executes fr's deferred function calls in LIFO order.
//
// On entry, fr.panicking indicates a state of panic; if
// true, fr.panic contains the panic value.
//
// On completion, if a deferred call started a panic, or if no
// deferred call recovered from a previous state of panic, then
// runDefers itself panics after the last deferred call has run.
//
// If there was no initial state of panic, or it was recovered from,
// runDefers returns normally.
//
func (fr *frame) runDefers() {
	for d := fr.defers; d != nil; d = d.tail {
		fr.runDefer(d)
	}
	fr.defers = nil
	// runtime.Goexit() fr.panic == nil
	if fr.panicking && fr.panic != nil {
		panic(fr.panic) // new panic, or still panicking
	}
}

// lookupMethod returns the method set for type typ, which may be one
// of the interpreter's fake types.
func lookupMethod(i *Interp, typ types.Type, meth *types.Func) *ssa.Function {
	return i.prog.LookupMethod(typ, meth.Pkg(), meth.Name())
}

func SetValue(v reflect.Value, x reflect.Value) {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(x.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(x.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(x.Uint())
	case reflect.Uintptr:
		v.SetUint(x.Uint())
	case reflect.Float32, reflect.Float64:
		v.SetFloat(x.Float())
	case reflect.Complex64, reflect.Complex128:
		v.SetComplex(x.Complex())
	case reflect.String:
		v.SetString(x.String())
	case reflect.UnsafePointer:
		v.SetPointer(unsafe.Pointer(x.Pointer()))
	default:
		v.Set(x)
	}
}

func hasUnderscore(st *types.Struct) bool {
	n := st.NumFields()
	for i := 0; i < n; i++ {
		if st.Field(i).Name() == "_" {
			return true
		}
	}
	return false
}

type DebugInfo struct {
	*ssa.DebugRef
	fset    *token.FileSet
	toValue func() (*types.Var, interface{}, bool) // var object value
}

func (i *DebugInfo) Position() token.Position {
	return i.fset.Position(i.Pos())
}

func (i *DebugInfo) AsVar() (*types.Var, interface{}, bool) {
	return i.toValue()
}

func (i *DebugInfo) AsFunc() (*types.Func, bool) {
	v, ok := i.Object().(*types.Func)
	return v, ok
}

/*
// visitInstr interprets a single ssa.Instruction within the activation
// record frame.  It returns a continuation value indicating where to
// read the next instruction from.
func (i *Interp) visitInstr(fr *frame, instr ssa.Instruction) (func(), continuation) {
	if i.mode&EnableDumpInstr != 0 {
		log.Printf("Instr %T %+v\n", instr, instr)
	}
	switch instr := instr.(type) {
	case *ssa.DebugRef:
		// no-op
		if i.fnDebug != nil {
			ref := &DebugInfo{DebugRef: instr, fset: i.fset}
			ref.toValue = func() (*types.Var, interface{}, bool) {
				if v, ok := instr.Object().(*types.Var); ok {
					return v, fr.get(instr.X), true
				}
				return nil, nil, false
			}
			i.fnDebug(ref)
		}
	case *ssa.UnOp:
		fr.env[instr] = unop(instr, fr.get(instr.X))

	case *ssa.BinOp:
		if instr.Op == token.SHR || instr.Op == token.SHL {
			if c, ok := instr.Y.(*ssa.Convert); ok {
				v := reflect.ValueOf(fr.get(c.X))
				vk := v.Kind()
				if vk >= reflect.Int && vk <= reflect.Int64 {
					if v.Int() < 0 {
						panic(runtimeError("negative shift amount"))
					}
				}
			}
		}
		fr.env[instr] = binop(instr, instr.X.Type(), fr.get(instr.X), fr.get(instr.Y))

	case *ssa.Call:
		return func() {
			fn, args := i.prepareCall(fr, &instr.Call)
			fr.env[instr] = i.call(fr, instr.Pos(), fn, args, instr.Call.Args)
		}, kNext

	case *ssa.ChangeInterface:
		fr.env[instr] = fr.get(instr.X)

	case *ssa.ChangeType:
		typ := i.toType(instr.Type())
		x := fr.get(instr.X)
		var v reflect.Value
		switch f := x.(type) {
		case *ssa.Function:
			v = i.makeFunc(fr, i.toType(f.Type()), f)
		default:
			v = reflect.ValueOf(x)
		}
		if !v.IsValid() {
			fr.env[instr] = reflect.New(typ).Elem()
		} else {
			fr.env[instr] = v.Convert(typ).Interface()
		}
		//fr.env[instr] = fr.get(instr.X)

	case *ssa.Convert:
		typ := i.toType(instr.Type())
		x := fr.get(instr.X)
		fr.env[instr] = convert(x, typ)
		//fr.env[instr] = conv(i, instr.Type(), instr.X.Type(), fr.get(instr.X))

	case *ssa.MakeInterface:
		typ := i.toType(instr.Type())
		v := reflect.New(typ).Elem()
		xtyp := i.toType(instr.X.Type())
		x := fr.get(instr.X)
		var vx reflect.Value
		switch x := x.(type) {
		case *ssa.Function:
			vx = i.makeFunc(fr, xtyp, x)
		case nil:
			vx = reflect.New(xtyp).Elem()
		default:
			vx = reflect.ValueOf(x)
			if xtyp != vx.Type() {
				vx = reflect.ValueOf(convert(x, xtyp))
			}
		}
		SetValue(v, vx)
		fr.env[instr] = v.Interface()
		//fr.env[instr] = iface{t: instr.X.Type(), v: fr.get(instr.X)}

	case *ssa.Extract:
		fr.env[instr] = fr.get(instr.Tuple).(tuple)[instr.Index]

	case *ssa.Slice:
		fr.env[instr] = slice(fr, instr)

	case *ssa.Return:
		switch len(instr.Results) {
		case 0:
		case 1:
			fr.result = fr.get(instr.Results[0])
		default:
			var res []value
			for _, r := range instr.Results {
				res = append(res, fr.get(r))
			}
			fr.result = tuple(res)
		}
		fr.block = nil
		return nil, kReturn

	case *ssa.RunDefers:
		fr.runDefers()

	case *ssa.Panic:
		panic(targetPanic{fr.get(instr.X)})

	case *ssa.Send:
		c := fr.get(instr.Chan)
		x := fr.get(instr.X)
		ch := reflect.ValueOf(c)
		if x == nil {
			ch.Send(reflect.New(ch.Type().Elem()).Elem())
		} else {
			ch.Send(reflect.ValueOf(x))
		}
		//fr.get(instr.Chan).(chan value) <- fr.get(instr.X)

	case *ssa.Store:
		// skip struct field _
		if addr, ok := instr.Addr.(*ssa.FieldAddr); ok {
			if s, ok := addr.X.Type().(*types.Pointer).Elem().(*types.Struct); ok {
				if s.Field(addr.Field).Name() == "_" {
					break
				}
			}
		}
		x := reflect.ValueOf(fr.get(instr.Addr))
		val := fr.get(instr.Val)
		switch fn := val.(type) {
		case *ssa.Function:
			f := i.makeFunc(fr, i.toType(fn.Type()), fn)
			SetValue(x.Elem(), f)
		default:
			v := reflect.ValueOf(val)
			if v.IsValid() {
				SetValue(x.Elem(), v)
			} else {
				SetValue(x.Elem(), reflect.New(x.Elem().Type()).Elem())
			}
		}
		//store(deref(instr.Addr.Type()), fr.get(instr.Addr).(*value), fr.get(instr.Val))

	case *ssa.If:
		succ := 1
		if v := fr.get(instr.Cond); reflect.ValueOf(v).Bool() {
			succ = 0
		}
		fr.prevBlock, fr.block = fr.block, fr.block.Succs[succ]
		return nil, kJump

	case *ssa.Jump:
		fr.prevBlock, fr.block = fr.block, fr.block.Succs[0]
		return nil, kJump

	case *ssa.Defer:
		fn, args := i.prepareCall(fr, &instr.Call)
		fr.defers = &deferred{
			fn:      fn,
			args:    args,
			ssaArgs: instr.Call.Args,
			instr:   instr,
			tail:    fr.defers,
		}

	case *ssa.Go:
		fn, args := i.prepareCall(fr, &instr.Call)
		atomic.AddInt32(&i.goroutines, 1)
		go func() {
			i.call(nil, instr.Pos(), fn, args, instr.Call.Args)
			atomic.AddInt32(&i.goroutines, -1)
		}()

	case *ssa.MakeChan:
		typ := i.toType(instr.Type())
		size := fr.get(instr.Size)
		buffer := asInt(size)
		if buffer < 0 {
			panic(runtimeError("makechan: size out of range"))
		}
		fr.env[instr] = reflect.MakeChan(typ, buffer).Interface()
		//fr.env[instr] = make(chan value, asInt(fr.get(instr.Size)))

	case *ssa.Alloc:
		typ := i.toType(instr.Type()).Elem() //deref(instr.Type()))
		//var addr *value
		if instr.Heap {
			// new
			//addr = new(value)
			//fr.env[instr] = addr
			fr.env[instr] = reflect.New(typ).Interface()
		} else {
			//fr.env[instr] = fr.locals[instr]
			// local
			//addr = fr.env[instr].(*value)
			v := reflect.ValueOf(fr.env[instr])
			SetValue(v.Elem(), fr.locals[instr])
			//SetValue(v.Elem(), reflect.Zero(typ))
		}
		//*addr = zero(deref(instr.Type()))

	case *ssa.MakeSlice:
		typ := i.toType(instr.Type())
		Len := asInt(fr.get(instr.Len))
		if Len < 0 || Len >= maxMemLen {
			panic(runtimeError("makeslice: len out of range"))
		}
		Cap := asInt(fr.get(instr.Cap))
		if Cap < 0 || Cap >= maxMemLen {
			panic(runtimeError("makeslice: cap out of range"))
		}
		fr.env[instr] = reflect.MakeSlice(typ, Len, Cap).Interface()
		// slice := make([]value, asInt(fr.get(instr.Cap)))
		// tElt := instr.Type().Underlying().(*types.Slice).Elem()
		// for i := range slice {
		// 	slice[i] = zero(tElt)
		// }
		// fr.env[instr] = slice[:asInt(fr.get(instr.Len))]

	case *ssa.MakeMap:
		typ := instr.Type()
		reserve := 0
		if instr.Reserve != nil {
			reserve = asInt(fr.get(instr.Reserve))
		}
		key := typ.Underlying().(*types.Map).Key()
		if st, ok := key.Underlying().(*types.Struct); ok && hasUnderscore(st) {
			fr.mapUnderscoreKey[typ] = true
		}
		fr.env[instr] = reflect.MakeMapWithSize(i.toType(typ), reserve).Interface()
		//fr.env[instr] = makeMap(instr.Type().Underlying().(*types.Map).Key(), reserve)

	case *ssa.Range:
		fr.env[instr] = rangeIter(fr.get(instr.X), instr.X.Type())

	case *ssa.Next:
		fr.env[instr] = fr.get(instr.Iter).(iter).next()

	case *ssa.FieldAddr:
		// v := reflect.ValueOf(fr.get(instr.X)).Elem()
		// fr.env[instr] = reflectx.Field(v, instr.Field).Addr().Interface()
		//fr.env[instr] = &(*fr.get(instr.X).(*value)).(structure)[instr.Field]
		v, err := FieldAddr(fr.get(instr.X), instr.Field)
		if err != nil {
			panic(runtimeError(err.Error()))
		}
		fr.env[instr] = v
	case *ssa.Field:
		// v := reflect.ValueOf(fr.get(instr.X))
		// for v.Kind() == reflect.Ptr {
		// 	v = v.Elem()
		// }
		//fr.env[instr] = reflectx.Field(v, instr.Field).Interface()
		//fr.env[instr] = fr.get(instr.X).(structure)[instr.Field]
		v, err := Field(fr.get(instr.X), instr.Field)
		if err != nil {
			panic(runtimeError(err.Error()))
		}
		fr.env[instr] = v

	case *ssa.IndexAddr:
		x := fr.get(instr.X)
		idx := fr.get(instr.Index)
		v := reflect.ValueOf(x)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		switch v.Kind() {
		case reflect.Slice:
		case reflect.Array:
		case reflect.Invalid:
			panic(runtimeError("invalid memory address or nil pointer dereference"))
		default:
			panic(fmt.Sprintf("unexpected x type in IndexAddr: %T", x))
		}
		index := asInt(idx)
		if index < 0 {
			panic(runtimeError(fmt.Sprintf("index out of range [%v]", index)))
		} else if length := v.Len(); index >= length {
			panic(runtimeError(fmt.Sprintf("index out of range [%v] with length %v", index, length)))
		}
		fr.env[instr] = v.Index(index).Addr().Interface()
		// switch x := x.(type) {
		// case []value:
		// 	fr.env[instr] = &x[asInt(idx)]
		// case *value: // *array
		// 	fr.env[instr] = &(*x).(array)[asInt(idx)]
		// default:
		// 	panic(fmt.Sprintf("unexpected x type in IndexAddr: %T", x))
		// }

	case *ssa.Index:
		x := fr.get(instr.X)
		idx := fr.get(instr.Index)
		v := reflect.ValueOf(x)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		fr.env[instr] = v.Index(asInt(idx)).Interface()

	case *ssa.Lookup:
		m := fr.get(instr.X)
		idx := fr.get(instr.Index)
		if s, ok := m.(string); ok {
			fr.env[instr] = s[asInt(idx)]
		} else {
			vm := reflect.ValueOf(m)
			v := vm.MapIndex(reflect.ValueOf(idx))
			ok := v.IsValid()
			var rv value
			if ok {
				rv = v.Interface()
			} else {
				rv = reflect.New(vm.Type().Elem()).Elem().Interface()
			}
			if instr.CommaOk {
				fr.env[instr] = tuple{rv, ok}
			} else {
				fr.env[instr] = rv
			}
		}
		//fr.env[instr] = lookup(instr, fr.get(instr.X), fr.get(instr.Index))

	case *ssa.MapUpdate:
		vm := reflect.ValueOf(fr.get(instr.Map))
		vk := reflect.ValueOf(fr.get(instr.Key))
		v := fr.get(instr.Value)
		if fn, ok := v.(*ssa.Function); ok {
			typ := i.toType(fn.Type())
			v = i.makeFunc(fr, typ, fn).Interface()
		}
		if fr.mapUnderscoreKey[instr.Map.Type()] {
			for _, vv := range vm.MapKeys() {
				if equalStruct(vk, vv) {
					vk = vv
					break
				}
			}
		}
		vm.SetMapIndex(vk, reflect.ValueOf(v))

	case *ssa.TypeAssert:
		v := fr.get(instr.X)
		if fn, ok := v.(*ssa.Function); ok {
			typ := i.toType(fn.Type())
			v = i.makeFunc(fr, typ, fn).Interface()
		}
		fr.env[instr] = typeAssert(i, instr, i.toType(instr.AssertedType), v)

	case *ssa.MakeClosure:
		var bindings []value
		for _, binding := range instr.Bindings {
			bindings = append(bindings, fr.get(binding))
		}
		//fr.env[instr] = &closure{instr.Fn.(*ssa.Function), bindings}
		c := &closure{instr.Fn.(*ssa.Function), bindings}
		typ := i.toType(c.Fn.Type())
		fr.env[instr] = i.makeFuncEx(fr, typ, c.Fn, c.Env).Interface()

	case *ssa.Phi:
		for i, pred := range instr.Block().Preds {
			if fr.prevBlock == pred {
				fr.env[instr] = fr.get(instr.Edges[i])
				break
			}
		}

	case *ssa.Select:
		var cases []reflect.SelectCase
		if !instr.Blocking {
			cases = append(cases, reflect.SelectCase{
				Dir: reflect.SelectDefault,
			})
		}
		for _, state := range instr.States {
			var dir reflect.SelectDir
			if state.Dir == types.RecvOnly {
				dir = reflect.SelectRecv
			} else {
				dir = reflect.SelectSend
			}
			ch := reflect.ValueOf(fr.get(state.Chan))
			var send reflect.Value
			if state.Send != nil {
				v := fr.get(state.Send)
				if v == nil {
					send = reflect.New(ch.Type().Elem()).Elem()
				} else {
					send = reflect.ValueOf(v)
				}
			}
			cases = append(cases, reflect.SelectCase{
				Dir:  dir,
				Chan: ch,
				Send: send,
			})
		}
		chosen, recv, recvOk := reflect.Select(cases)
		if !instr.Blocking {
			chosen-- // default case should have index -1.
		}
		r := tuple{chosen, recvOk}
		for n, st := range instr.States {
			if st.Dir == types.RecvOnly {
				var v value
				if n == chosen && recvOk {
					// No need to copy since send makes an unaliased copy.
					v = recv.Interface()
				} else {
					typ := i.toType(st.Chan.Type())
					v = reflect.New(typ.Elem()).Elem().Interface()
					//v = zero(st.Chan.Type().Underlying().(*types.Chan).Elem())
				}
				r = append(r, v)
			}
		}
		fr.env[instr] = r
	case *ssa.SliceToArrayPointer:
		typ := i.toType(instr.Type())
		x := fr.get(instr.X)
		v := reflect.ValueOf(x)
		vLen := v.Len()
		tLen := typ.Elem().Len()
		if tLen > vLen {
			panic(runtimeError(fmt.Sprintf("cannot convert slice with length %v to pointer to array with length %v", vLen, tLen)))
		}
		fr.env[instr] = v.Convert(typ).Interface()
	default:
		panic(fmt.Sprintf("unexpected instruction: %T", instr))
	}

	// if val, ok := instr.(ssa.Value); ok {
	// 	fmt.Println(toString(fr.env[val])) // debugging
	// }

	return nil, kNext
}
*/

// prepareCall determines the function value and argument values for a
// function call in a Call, Go or Defer instruction, performing
// interface method lookup if needed.
//
func (i *Interp) prepareCall(fr *frame, call *ssa.CallCommon) (fn value, args []value) {
	v := fr.get(call.Value)
	if call.Method == nil {
		// Function call.
		fn = v
	} else {
		// Interface method invocation.
		//vt, ok := call.Value.(*ssa.MakeInterface)
		// recv := v.(iface)
		rv := reflect.ValueOf(v)
		t, ok := i.findType(rv.Type(), true)
		if ok {
			if f := lookupMethod(i, t, call.Method); f == nil {
				// Unreachable in well-typed programs.
				panic(fmt.Sprintf("method set for dynamic type %v does not contain %s", t, call.Method))
			} else {
				fn = f
			}
		} else {
			rtype := rv.Type()
			mname := call.Method.Name()
			if s := rtype.String(); (s == "*reflect.rtype" || s == "reflect.Type") &&
				mname == "Method" || mname == "MethodByName" {
				if mname == "Method" {
					fn = reflectx.MethodByIndex
				} else {
					fn = reflectx.MethodByName
				}
			} else {
				if f, ok := rtype.MethodByName(mname); ok {
					fn = f.Func.Interface()
				} else {
					panic(runtimeError("invalid memory address or nil pointer dereference"))
				}
			}
		}
		args = append(args, v)
	}
	for _, arg := range call.Args {
		v := fr.get(arg)
		if fn, ok := v.(*ssa.Function); ok {
			v = i.makeFunc(fr, i.toType(fn.Type()), fn).Interface()
		}
		args = append(args, v)
	}
	return
}

// call interprets a call to a function (function, builtin or closure)
// fn with arguments args, returning its result.
// callpos is the position of the callsite.
//
func (i *Interp) call(caller *frame, callpos token.Pos, fn value, args []value, ssaArgs []ssa.Value) value {
	if caller == nil {
		caller = i.caller
	} else {
		i.caller = caller
	}
	switch fn := fn.(type) {
	case *ssa.Function:
		if fn == nil {
			panic("call of nil function") // nil of func type
		}
		return i.callSSA(caller, callpos, fn, args, nil)
	case *closure:
		if fn.Fn == nil {
			panic("call of nil closure function") // nil of func type
		}
		return i.callSSA(caller, callpos, fn.Fn, args, fn.Env)
	case *ssa.Builtin:
		return i.callBuiltin(caller, callpos, fn, args, ssaArgs)
	default:
		if f := reflect.ValueOf(fn); f.Kind() == reflect.Func {
			return i.callReflect(caller, callpos, f, args, nil)
		}
	}
	panic(fmt.Sprintf("cannot call %T %v", fn, reflect.ValueOf(fn).Kind()))
}

func loc(fset *token.FileSet, pos token.Pos) string {
	if pos == token.NoPos {
		return ""
	}
	return " at " + fset.Position(pos).String()
}

func (i *Interp) callFunction(caller *frame, callpos token.Pos, fn *ssa.Function, args []value, env []value) value {
	fr := &frame{
		i:      i,
		caller: caller, // for panic/recover
		pfn:    i.funcs[fn],
	}
	//fr.stack = make([]value, fr.pfn.valueCount, fr.pfn.valueCount)
	//copy(fr.stack, fr.pfn.values)
	fr.stack = append([]value{}, fr.pfn.values...)
	// fr.env = make(map[ssa.Value]value)
	fr.block = fr.pfn.MainBlock
	// fr.locals = make(map[ssa.Value]reflect.Value)
	//fr.mapUnderscoreKey = make(map[types.Type]bool)
	var index int
	for _, l := range fn.Locals {
		_ = l
		typ := i.toType(deref(l.Type()))
		// fr.locals[l] = reflect.New(typ).Elem()   //zero(deref(l.Type()))
		// fr.env[l] = reflect.New(typ).Interface() //&fr.locals[i]
		fr.stack[index] = reflect.New(typ).Interface()
		index++
	}
	for i, p := range fn.Params {
		_ = p
		// fr.env[p] = args[i]
		fr.stack[index] = args[i]
		index++
	}
	for i, fv := range fn.FreeVars {
		_ = fv
		// fr.env[fv] = env[i]
		fr.stack[index] = env[i]
		index++
	}
	for fr.block != nil {
		i.runFrame(fr)
	}
	// Destroy the locals to avoid accidental use after return.
	//log.Println("======", fr.stack)
	//fr.env = nil
	fr.block = nil
	//fr.locals = nil
	return fr.result
}

func (i *Interp) callFunctionEx(caller *frame, callpos token.Pos, fn *ssa.Function, pfn *Function, args []value, env []value) value {
	fr := &frame{
		i:      i,
		caller: caller, // for panic/recover
		pfn:    pfn,
	}
	//fr.stack = make([]value, fr.pfn.valueCount, fr.pfn.valueCount)
	//copy(fr.stack, fr.pfn.values)
	fr.stack = append([]value{}, fr.pfn.values...)
	// fr.env = make(map[ssa.Value]value)
	fr.block = fr.pfn.MainBlock
	// fr.locals = make(map[ssa.Value]reflect.Value)
	//fr.mapUnderscoreKey = make(map[types.Type]bool)
	var index int
	for _, l := range fn.Locals {
		_ = l
		typ := i.toType(deref(l.Type()))
		// fr.locals[l] = reflect.New(typ).Elem()   //zero(deref(l.Type()))
		// fr.env[l] = reflect.New(typ).Interface() //&fr.locals[i]
		fr.stack[index] = reflect.New(typ).Interface()
		index++
	}
	for i, p := range fn.Params {
		_ = p
		// fr.env[p] = args[i]
		fr.stack[index] = args[i]
		index++
	}
	for i, fv := range fn.FreeVars {
		_ = fv
		// fr.env[fv] = env[i]
		fr.stack[index] = env[i]
		index++
	}
	for fr.block != nil {
		i.runFrame(fr)
	}
	// Destroy the locals to avoid accidental use after return.
	//log.Println("======", fr.stack)
	//fr.env = nil
	fr.block = nil
	//fr.locals = nil
	return fr.result
}

// callSSA interprets a call to function fn with arguments args,
// and lexical environment env, returning its result.
// callpos is the position of the callsite.
//
func (i *Interp) callSSA(caller *frame, callpos token.Pos, fn *ssa.Function, args []value, env []value) value {
	if i.mode&EnableTracing != 0 {
		fset := fn.Prog.Fset
		// TODO(adonovan): fix: loc() lies for external functions.
		log.Printf("Entering %s%s.\n", fn, loc(fset, fn.Pos()))
		suffix := ""
		if caller != nil {
			suffix = ", resuming " + caller.pfn.Fn.String() + loc(fset, callpos)
		}
		defer log.Printf("Leaving %s%s.\n", fn, suffix)
	}
	if fn.Parent() == nil {
		fullName := fn.String()
		name := fn.Name()
		if ext := externValues[fullName]; ext.Kind() == reflect.Func {
			if i.mode&EnableTracing != 0 {
				log.Println("\t(external)")
			}
			return i.callReflect(caller, callpos, ext, args, nil)
		}
		if fn.Pkg != nil {
			pkgPath := fn.Pkg.Pkg.Path()
			if pkg, ok := i.installed(pkgPath); ok {
				if recv := fn.Signature.Recv(); recv == nil {
					if ext, ok := pkg.Funcs[name]; ok {
						if i.mode&EnableTracing != 0 {
							log.Println("\t(external func)")
						}
						return i.callReflect(caller, callpos, ext, args, nil)
					}
				} else if typ, ok := i.loader.LookupReflect(recv.Type()); ok {
					//TODO maybe make full name for search
					if m, ok := typ.MethodByName(fn.Name()); ok {
						if i.mode&EnableTracing != 0 {
							log.Println("\t(external reflect method)")
						}
						return i.callReflect(caller, callpos, m.Func, args, nil)
					}
					// if ext, ok := pkg.Methods[fullName]; ok {
					// 	if i.mode&EnableTracing != 0 {
					// 		log.Println("\t(external method)")
					// 	}
					// 	return i.callReflect(caller, callpos, ext, args, nil)
					// }
				}
			}
		}
		if fn.Blocks == nil {
			// check unexport method
			if fn.Signature.Recv() != nil {
				v := reflect.ValueOf(args[0])
				if f, ok := v.Type().MethodByName(fn.Name()); ok {
					return i.callReflect(caller, callpos, f.Func, args, nil)
				}
			}
			if fn.Name() == "init" && fn.Type().String() == "func()" {
				return true
			}
			panic("no code for function: " + fullName)
		}
	}
	fr := &frame{
		i:      i,
		caller: caller, // for panic/recover
		pfn:    i.funcs[fn],
	}
	fr.stack = make([]value, fr.pfn.valueCount, fr.pfn.valueCount)
	copy(fr.stack, fr.pfn.values)

	fr.env = make(map[ssa.Value]value)
	fr.block = fr.pfn.MainBlock
	fr.locals = make(map[ssa.Value]reflect.Value)
	fr.mapUnderscoreKey = make(map[types.Type]bool)
	var index int
	for _, l := range fn.Locals {
		typ := i.toType(deref(l.Type()))
		fr.locals[l] = reflect.New(typ).Elem()   //zero(deref(l.Type()))
		fr.env[l] = reflect.New(typ).Interface() //&fr.locals[i]
		fr.stack[index] = reflect.New(typ).Interface()
		index++
	}
	for i, p := range fn.Params {
		fr.env[p] = args[i]
		fr.stack[index] = args[i]
		index++
	}
	for i, fv := range fn.FreeVars {
		fr.env[fv] = env[i]
		fr.stack[index] = env[i]
		index++
	}
	for fr.block != nil {
		i.runFrame(fr)
	}
	// Destroy the locals to avoid accidental use after return.
	fr.env = nil
	fr.block = nil
	fr.locals = nil
	return fr.result
}

func (i *Interp) callReflect(caller *frame, callpos token.Pos, fn reflect.Value, args []value, env []value) value {
	i.caller = caller
	var ins []reflect.Value
	typ := fn.Type()
	isVariadic := fn.Type().IsVariadic()
	if isVariadic {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == nil {
				ins = append(ins, reflect.New(typ.In(i)).Elem())
			} else {
				ins = append(ins, reflect.ValueOf(args[i]))
			}
		}
		ins = append(ins, reflect.ValueOf(args[len(args)-1]))
	} else {
		ins = make([]reflect.Value, len(args), len(args))
		for i := 0; i < len(args); i++ {
			if args[i] == nil {
				ins[i] = reflect.New(typ.In(i)).Elem()
			} else {
				ins[i] = reflect.ValueOf(args[i])
			}
		}
	}
	var results []reflect.Value
	if isVariadic {
		results = fn.CallSlice(ins)
	} else {
		results = fn.Call(ins)
	}
	switch len(results) {
	case 0:
		return nil
	case 1:
		return results[0].Interface()
	default:
		var res []value
		for _, r := range results {
			res = append(res, r.Interface())
		}
		return tuple(res)
	}
}

// runFrame executes SSA instructions starting at fr.block and
// continuing until a return, a panic, or a recovered panic.
//
// After a panic, runFrame panics.
//
// After a normal return, fr.result contains the result of the call
// and fr.block is nil.
//
// A recovered panic in a function without named return parameters
// (NRPs) becomes a normal return of the zero value of the function's
// result type.
//
// After a recovered panic in a function with NRPs, fr.result is
// undefined and fr.block contains the block at which to resume
// control.
//
func (i *Interp) runFrame(fr *frame) {
	// if fr.pfn.Recover != nil {
	// 	defer func() {
	// 		if fr.block == nil {
	// 			return // normal return
	// 		}
	// 		if i.mode&DisableRecover != 0 {
	// 			return // let interpreter crash
	// 		}
	// 		fr.panicking = true
	// 		fr.panic = recover()
	// 		if i.mode&EnableTracing != 0 {
	// 			log.Printf("Panicking: %T %v.\n", fr.panic, fr.panic)
	// 		}
	// 		fr.runDefers()
	// 		fr.block = fr.pfn.Recover
	// 	}()
	// }

	for {
		// if i.mode&EnableTracing != 0 {
		// 	log.Printf(".%v:\n", fr.block.Index)
		// }
	block:
		for _, fn := range fr.block.Instrs {
			var k int
			fn(fr, &k)
			switch k {
			case kReturn:
				return
			case kJump:
				break block
			}
		}

		// for _, instr := range fr.block.Instrs {
		// 	if i.mode&EnableTracing != 0 {
		// 		if v, ok := instr.(ssa.Value); ok {
		// 			log.Println("\t", v.Name(), "=", instr)
		// 		} else {
		// 			log.Println("\t", instr)
		// 		}
		// 	}
		// 	fn, cond := i.visitInstr(fr, instr)
		// 	if fn != nil {
		// 		fn()
		// 	}
		// 	switch cond {
		// 	case kReturn:
		// 		return
		// 	case kNext:
		// 		// no-op
		// 	case kJump:
		// 		break block
		// 	}
		// }

	}
}

// doRecover implements the recover() built-in.
func doRecover(caller *frame) value {
	// recover() must be exactly one level beneath the deferred
	// function (two levels beneath the panicking function) to
	// have any effect.  Thus we ignore both "defer recover()" and
	// "defer f() -> g() -> recover()".
	if caller.i.mode&DisableRecover == 0 &&
		caller != nil && !caller.panicking &&
		caller.caller != nil && caller.caller.panicking {
		caller.caller.panicking = false
		p := caller.caller.panic
		caller.caller.panic = nil
		// TODO(adonovan): support runtime.Goexit.
		switch p := p.(type) {
		case targetPanic:
			// The target program explicitly called panic().
			return p.v
		case runtime.Error:
			// The interpreter encountered a runtime error.
			return p
			//return iface{caller.i.runtimeErrorString, p.Error()}
		case string:
			return p
		case plainError:
			return p
		case runtimeError:
			return p
		case *reflect.ValueError:
			return p
		default:
			panic(fmt.Sprintf("unexpected panic type %T in target call to recover()", p))
		}
	}
	return nil //iface{}
}

// setGlobal sets the value of a system-initialized global variable.
func setGlobal(i *Interp, pkg *ssa.Package, name string, v value) {
	// if g, ok := i.globals[pkg.Var(name)]; ok {
	// 	*g = v
	// 	return
	// }
	panic("no global variable: " + pkg.Pkg.Path() + "." + name)
}

// Interpret interprets the Go program whose main package is mainpkg.
// mode specifies various interpreter options.  filename and args are
// the initial values of os.Args for the target program.  sizes is the
// effective type-sizing function for this program.
//
// Interpret returns the exit code of the program: 2 for panic (like
// gc does), or the argument to os.Exit for normal termination.
//
// The SSA program must include the "runtime" package.
//

func NewInterp(loader Loader, mainpkg *ssa.Package, mode Mode) (*Interp, error) {
	i := &Interp{
		fset:         mainpkg.Prog.Fset,
		prog:         mainpkg.Prog,
		mainpkg:      mainpkg,
		globals:      make(map[ssa.Value]value),
		mode:         mode,
		goroutines:   1,
		preloadTypes: make(map[types.Type]reflect.Type),
		funcs:        make(map[*ssa.Function]*Function),
	}
	i.loader = loader
	i.record = NewTypesRecord(i.loader, i)
	i.record.Load(mainpkg)

	// Initialize global storage.
	for _, m := range mainpkg.Members {
		switch v := m.(type) {
		case *ssa.Global:
			typ := i.preToType(deref(v.Type()))
			i.globals[v] = reflect.New(typ).Interface()
		}
	}
	// static types check
	err := checkPackages(i, []*ssa.Package{mainpkg})
	if err != nil {
		return i, err
	}

	_, err = i.Run("init")
	if err != nil {
		err = fmt.Errorf("init error: %w", err)
	}
	return i, err
}

func (i *Interp) loadType(typ types.Type) {
	if _, ok := i.preloadTypes[typ]; !ok {
		i.preloadTypes[typ] = i.record.ToType(typ)
	}
}

func (i *Interp) preToType(typ types.Type) reflect.Type {
	if t, ok := i.preloadTypes[typ]; ok {
		return t
	}
	t := i.record.ToType(typ)
	i.preloadTypes[typ] = t
	return t
}

func (i *Interp) toType(typ types.Type) reflect.Type {
	if t, ok := i.preloadTypes[typ]; ok {
		return t
	}
	//log.Panicf("toType %v %p\n", typ, typ)
	i.typesMutex.Lock()
	defer i.typesMutex.Unlock()
	return i.record.ToType(typ)
}

func (i *Interp) RunFunc(name string, args ...Value) (r Value, err error) {
	defer func() {
		if i.mode&DisableRecover != 0 {
			return
		}
		switch p := recover().(type) {
		case nil:
			// nothing
		case exitPanic:
			// nothing
		case targetPanic:
			err = p
		case runtime.Error:
			err = p
		case string:
			err = plainError(p)
		case plainError:
			err = p
		default:
			err = fmt.Errorf("unexpected type: %T: %v", p, p)
		}
	}()
	if fn := i.mainpkg.Func(name); fn != nil {
		r = i.call(nil, token.NoPos, fn, args, nil)
	} else {
		err = fmt.Errorf("no function %v", name)
	}
	return
}

func (i *Interp) Run(entry string) (exitCode int, err error) {
	// Top-level error handler.
	exitCode = 2
	defer func() {
		if exitCode != 2 || i.mode&DisableRecover != 0 {
			return
		}
		switch p := recover().(type) {
		case nil:
			// nothing
		case exitPanic:
			exitCode = int(p)
		case targetPanic:
			err = p
		case runtime.Error:
			err = p
		case string:
			err = plainError(p)
		case plainError:
			err = p
		default:
			err = fmt.Errorf("unexpected type: %T: %v", p, p)
		}
	}()
	if mainFn := i.mainpkg.Func(entry); mainFn != nil {
		i.call(nil, token.NoPos, mainFn, nil, nil)
		exitCode = 0
	} else {
		err = fmt.Errorf("no function %v", entry)
		exitCode = 1
	}
	return
}

func (i *Interp) GetFunc(key string) (interface{}, bool) {
	m, ok := i.mainpkg.Members[key]
	if !ok {
		return nil, false
	}
	fn, ok := m.(*ssa.Function)
	if !ok {
		return nil, false
	}
	return i.makeFunc(nil, i.toType(fn.Type()), fn).Interface(), true
}

func (i *Interp) GetVarAddr(key string) (interface{}, bool) {
	m, ok := i.mainpkg.Members[key]
	if !ok {
		return nil, false
	}
	v, ok := m.(*ssa.Global)
	if !ok {
		return nil, false
	}
	p, ok := i.globals[v]
	return p, ok
}

func (i *Interp) GetConst(key string) (constant.Value, bool) {
	m, ok := i.mainpkg.Members[key]
	if !ok {
		return nil, false
	}
	v, ok := m.(*ssa.NamedConst)
	if !ok {
		return nil, false
	}
	return v.Value.Value, true
}

func (i *Interp) GetType(key string) (reflect.Type, bool) {
	m, ok := i.mainpkg.Members[key]
	if !ok {
		return nil, false
	}
	t, ok := m.(*ssa.Type)
	if !ok {
		return nil, false
	}
	return i.toType(t.Type()), true
}

// deref returns a pointer's element type; otherwise it returns typ.
// TODO(adonovan): Import from ssa?
func deref(typ types.Type) types.Type {
	if p, ok := typ.Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return typ
}
