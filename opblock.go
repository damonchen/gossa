package gossa

import (
	"fmt"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/goplus/reflectx"
	"golang.org/x/tools/go/ssa"
)

/*
type Op int

const (
	OpInvalid Op = iota
	// Value-defining instructions
	OpAlloc
	OpPhi
	OpCall
	OpBinOp
	OpUnOp
	OpChangeType
	OpConvert
	OpChangeInterface
	OpSliceToArrayPointer
	OpMakeInterface
	OpMakeClosure
	OpMakeMap
	OpMakeChan
	OpMakeSlice
	OpSlice
	OpFieldAddr
	OpField
	OpIndexAddr
	OpIndex
	OpLookup
	OpSelect
	OpRange
	OpNext
	OpTypeAssert
	OpExtract
	// Instructions executed for effect
	OpJump
	OpIf
	OpReturn
	OpRunDefers
	OpPanic
	OpGo
	OpDefer
	OpSend
	OpStore
	OpMapUpdate
	OpDebugRef
)
*/

type FuncBlock struct {
	Index        int
	Instrs       []func(fr *frame, k *int)
	Preds, Succs []*FuncBlock
}

type Function struct {
	intp       *Interp
	Fn         *ssa.Function
	Blocks     map[*ssa.BasicBlock]*FuncBlock
	MainBlock  *FuncBlock
	Recover    *FuncBlock
	valueIndex map[ssa.Value]int
	valueCount int
	values     []value
}

func (p *Function) getIndex(v ssa.Value) int {
	if i, ok := p.valueIndex[v]; ok {
		return i
	}
	i := p.valueCount
	p.valueIndex[v] = i
	p.valueCount++
	switch v := v.(type) {
	case *constValue:
		p.values = append(p.values, v.Value)
	case *ssa.Const:
		p.values = append(p.values, constToValue(p.intp, v))
	case *ssa.Global:
		g, _ := globalToValue(p.intp, v)
		p.values = append(p.values, g)
	default:
		p.values = append(p.values, nil)
	}
	return i
}

func findExternFunc(interp *Interp, fn *ssa.Function) (ext reflect.Value, ok bool) {
	ext, ok = externValues[fn.String()]
	if !ok && fn.Pkg != nil {
		if recv := fn.Signature.Recv(); recv == nil {
			if pkg, found := interp.installed(fn.Pkg.Pkg.Path()); found {
				ext, ok = pkg.Funcs[fn.Name()]
			}
		} else if typ, found := interp.loader.LookupReflect(recv.Type()); found {
			if m, found := reflectx.MethodByName(typ, fn.Name()); found {
				ext, ok = m.Func, true
			}
		}
	}
	return
}

func makeInstr(interp *Interp, pfn *Function, instr ssa.Instruction) func(fr *frame, k *int) {
	var index = -1
	if v, ok := instr.(ssa.Value); ok {
		index = pfn.getIndex(v)
	}
	_ = index
	switch instr := instr.(type) {
	case *ssa.Alloc:
		if instr.Heap {
			typ := interp.preToType(instr.Type()).Elem()
			return func(fr *frame, k *int) {
				fr.env[instr] = reflect.New(typ).Interface()
			}
		} else {
			return func(fr *frame, k *int) {
				v := reflect.ValueOf(fr.env[instr])
				SetValue(v.Elem(), fr.locals[instr])
			}
		}
	case *ssa.Phi:
		return func(fr *frame, k *int) {
			block := fr.pfn.Blocks[instr.Block()]
			for i, pred := range block.Preds {
				if fr.prevBlock == pred {
					fr.env[instr] = fr.get(instr.Edges[i])
					break
				}
			}
		}
	case *ssa.Call:
		return makeCallInstr(interp, pfn, instr, instr.Call)
	case *ssa.BinOp:
		//log.Println("binOp", instr, pfn.getIndex(instr.X), pfn.getIndex(instr.Y))
		i, ix, iy := pfn.getIndex(instr), pfn.getIndex(instr.X), pfn.getIndex(instr.Y)
		switch instr.Op {
		case token.ADD:
			return func(fr *frame, k *int) {
				// fr.env[instr] = opADD(fr.get(instr.X), fr.get(instr.Y))
				// fr.stack[i] = opADD(fr.stack[ix], fr.stack[iy])
				//fr.stack[i] = fr.reg(ix).(int) + fr.reg(iy).(int)
				fr.setReg(i, fr.reg(ix).(int)+fr.reg(iy).(int))
			}
		case token.SUB:
			return func(fr *frame, k *int) {
				// fr.env[instr] = opSUB(fr.get(instr.X), fr.get(instr.Y))
				// fr.stack[i] = opSUB(fr.stack[ix], fr.stack[iy])
				//fr.stack[i] = fr.reg(ix).(int) - fr.reg(iy).(int)
				fr.setReg(i, fr.reg(ix).(int)-fr.reg(iy).(int))
			}
		case token.MUL:
			return func(fr *frame, k *int) {
				fr.env[instr] = opMUL(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.QUO:
			return func(fr *frame, k *int) {
				fr.env[instr] = opQuo(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.REM:
			return func(fr *frame, k *int) {
				fr.env[instr] = opREM(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.AND:
			return func(fr *frame, k *int) {
				fr.env[instr] = opAND(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.OR:
			return func(fr *frame, k *int) {
				fr.env[instr] = opOR(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.XOR:
			return func(fr *frame, k *int) {
				fr.env[instr] = opXOR(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.AND_NOT:
			return func(fr *frame, k *int) {
				fr.env[instr] = opANDNOT(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.SHL:
			if c, ok := instr.Y.(*ssa.Convert); ok {
				xtyp := interp.preToType(c.X.Type())
				xk := xtyp.Kind()
				if xk >= reflect.Int && xk <= reflect.Int64 {
					return func(fr *frame, k *int) {
						v := reflect.ValueOf(fr.get(c.X))
						if v.Int() < 0 {
							panic(runtimeError("negative shift amount"))
						}
						fr.env[instr] = opSHL(fr.get(instr.X), fr.get(instr.Y))
					}
				}
			}
			return func(fr *frame, k *int) {
				fr.env[instr] = opSHL(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.SHR:
			if c, ok := instr.Y.(*ssa.Convert); ok {
				xtyp := interp.preToType(c.X.Type())
				xk := xtyp.Kind()
				if xk >= reflect.Int && xk <= reflect.Int64 {
					return func(fr *frame, k *int) {
						v := reflect.ValueOf(fr.get(c.X))
						if v.Int() < 0 {
							panic(runtimeError("negative shift amount"))
						}
						fr.env[instr] = opSHR(fr.get(instr.X), fr.get(instr.Y))
					}
				}
			}
			return func(fr *frame, k *int) {
				fr.env[instr] = opSHR(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.LSS:
			return func(fr *frame, k *int) {
				// fr.env[instr] = opLSS(fr.get(instr.X), fr.get(instr.Y))
				// fr.stack[i] = opLSS(fr.stack[ix], fr.stack[iy])
				//fr.stack[i] = fr.reg(ix).(int) < fr.reg(iy).(int)
				fr.setReg(i, fr.reg(ix).(int) < fr.reg(iy).(int))
			}
		case token.LEQ:
			return func(fr *frame, k *int) {
				fr.env[instr] = opLEQ(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.EQL:
			return func(fr *frame, k *int) {
				fr.env[instr] = opEQL(instr, fr.get(instr.X), fr.get(instr.Y))
			}
		case token.NEQ:
			return func(fr *frame, k *int) {
				fr.env[instr] = !opEQL(instr, fr.get(instr.X), fr.get(instr.Y))
			}
		case token.GTR:
			return func(fr *frame, k *int) {
				fr.env[instr] = opGTR(fr.get(instr.X), fr.get(instr.Y))
			}
		case token.GEQ:
			return func(fr *frame, k *int) {
				fr.env[instr] = opGEQ(fr.get(instr.X), fr.get(instr.Y))
			}
		default:
			panic(fmt.Errorf("unreachable %v", instr.Op))
		}
	case *ssa.UnOp:
		ii := pfn.getIndex(instr)
		ix := pfn.getIndex(instr.X)
		return func(fr *frame, k *int) {
			//fr.env[instr] = unop(instr, fr.get(instr.X))
			//fr.stack[ii] = unop(instr, fr.reg(ix))
			fr.setReg(ii, unop(instr, fr.reg(ix)))
		}
	case *ssa.ChangeInterface:
		return func(fr *frame, k *int) {
			fr.env[instr] = fr.get(instr.X)
		}
	case *ssa.ChangeType:
		typ := interp.preToType(instr.Type())
		switch f := instr.X.(type) {
		case *ssa.Function:
			fn := interp.makeFunc(nil, interp.preToType(f.Type()), f)
			v := fn.Convert(typ).Interface()
			return func(fr *frame, k *int) {
				fr.env[instr] = v
			}
		default:
			return func(fr *frame, k *int) {
				x := fr.get(instr.X)
				if x == nil {
					fr.env[instr] = reflect.New(typ).Elem().Interface()
				} else {
					fr.env[instr] = reflect.ValueOf(x).Convert(typ).Interface()
				}
			}
		}
	case *ssa.Convert:
		typ := interp.preToType(instr.Type())
		return func(fr *frame, k *int) {
			x := fr.get(instr.X)
			fr.env[instr] = convert(x, typ)
		}
	case *ssa.MakeInterface:
		typ := interp.preToType(instr.Type())
		xtyp := interp.preToType(instr.X.Type())
		switch f := instr.X.(type) {
		case *ssa.Function:
			fn := interp.makeFunc(nil, xtyp, f)
			return func(fr *frame, k *int) {
				v := reflect.New(typ).Elem()
				SetValue(v, fn)
				fr.env[instr] = v.Interface()
			}
		default:
			return func(fr *frame, k *int) {
				v := reflect.New(typ).Elem()
				x := fr.get(instr.X)
				var vx reflect.Value
				if x == nil {
					vx = reflect.New(xtyp).Elem()
				} else {
					vx = reflect.ValueOf(x)
					if xtyp != vx.Type() {
						vx = reflect.ValueOf(convert(x, xtyp))
					}
				}
				SetValue(v, vx)
				fr.env[instr] = v.Interface()
			}
		}
	case *ssa.MakeClosure:
		fn := instr.Fn.(*ssa.Function)
		typ := interp.preToType(fn.Type())
		return func(fr *frame, k *int) {
			var bindings []value
			for _, binding := range instr.Bindings {
				bindings = append(bindings, fr.get(binding))
			}
			c := &closure{fn, bindings}
			fr.env[instr] = interp.makeFuncEx(fr, typ, c.Fn, c.Env).Interface()
		}
	case *ssa.MakeChan:
		typ := interp.preToType(instr.Type())
		return func(fr *frame, k *int) {
			size := fr.get(instr.Size)
			buffer := asInt(size)
			if buffer < 0 {
				panic(runtimeError("makechan: size out of range"))
			}
			fr.env[instr] = reflect.MakeChan(typ, buffer).Interface()
		}
	case *ssa.MakeMap:
		typ := instr.Type()
		rtyp := interp.preToType(typ)
		key := typ.Underlying().(*types.Map).Key()
		var reserve int
		return func(fr *frame, k *int) {
			if instr.Reserve != nil {
				reserve = asInt(fr.get(instr.Reserve))
			}
			if st, ok := key.Underlying().(*types.Struct); ok && hasUnderscore(st) {
				fr.mapUnderscoreKey[typ] = true
			}
			fr.env[instr] = reflect.MakeMapWithSize(rtyp, reserve).Interface()
		}
	case *ssa.MakeSlice:
		typ := interp.preToType(instr.Type())
		return func(fr *frame, k *int) {
			Len := asInt(fr.get(instr.Len))
			if Len < 0 || Len >= maxMemLen {
				panic(runtimeError("makeslice: len out of range"))
			}
			Cap := asInt(fr.get(instr.Cap))
			if Cap < 0 || Cap >= maxMemLen {
				panic(runtimeError("makeslice: cap out of range"))
			}
			fr.env[instr] = reflect.MakeSlice(typ, Len, Cap).Interface()
		}
	case *ssa.Slice:
		return func(fr *frame, k *int) {
			fr.env[instr] = slice(fr, instr)
		}
	case *ssa.FieldAddr:
		return func(fr *frame, k *int) {
			v, err := FieldAddr(fr.get(instr.X), instr.Field)
			if err != nil {
				panic(runtimeError(err.Error()))
			}
			fr.env[instr] = v
		}
	case *ssa.Field:
		return func(fr *frame, k *int) {
			v, err := Field(fr.get(instr.X), instr.Field)
			if err != nil {
				panic(runtimeError(err.Error()))
			}
			fr.env[instr] = v
		}
	case *ssa.IndexAddr:
		return func(fr *frame, k *int) {
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
		}
	case *ssa.Index:
		return func(fr *frame, k *int) {
			x := fr.get(instr.X)
			idx := fr.get(instr.Index)
			v := reflect.ValueOf(x)
			if v.Kind() == reflect.Ptr {
				v = v.Elem()
			}
			fr.env[instr] = v.Index(asInt(idx)).Interface()
		}
	case *ssa.Lookup:
		typ := interp.preToType(instr.X.Type())
		switch typ.Kind() {
		case reflect.String:
			return func(fr *frame, k *int) {
				v := fr.get(instr.X)
				idx := fr.get(instr.Index)
				fr.env[instr] = reflect.ValueOf(v).String()[asInt(idx)]
			}
		case reflect.Map:
			return func(fr *frame, k *int) {
				m := fr.get(instr.X)
				idx := fr.get(instr.Index)
				vm := reflect.ValueOf(m)
				v := vm.MapIndex(reflect.ValueOf(idx))
				ok := v.IsValid()
				var rv value
				if ok {
					rv = v.Interface()
				} else {
					rv = reflect.New(typ.Elem()).Elem().Interface()
				}
				if instr.CommaOk {
					fr.env[instr] = tuple{rv, ok}
				} else {
					fr.env[instr] = rv
				}
			}
		default:
			panic("unreachable")
		}
	case *ssa.Select:
		return func(fr *frame, k *int) {
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
						typ := interp.toType(st.Chan.Type())
						v = reflect.New(typ.Elem()).Elem().Interface()
					}
					r = append(r, v)
				}
			}
			fr.env[instr] = r
		}
	case *ssa.SliceToArrayPointer:
		typ := interp.preToType(instr.Type())
		return func(fr *frame, k *int) {
			x := fr.get(instr.X)
			v := reflect.ValueOf(x)
			vLen := v.Len()
			tLen := typ.Elem().Len()
			if tLen > vLen {
				panic(runtimeError(fmt.Sprintf("cannot convert slice with length %v to pointer to array with length %v", vLen, tLen)))
			}
			fr.env[instr] = v.Convert(typ).Interface()
		}
	case *ssa.Range:
		typ := interp.preToType(instr.X.Type())
		switch typ.Kind() {
		case reflect.String:
			return func(fr *frame, k *int) {
				v := fr.get(instr.X)
				fr.env[instr] = &stringIter{Reader: strings.NewReader(reflect.ValueOf(v).String())}
			}
		case reflect.Map:
			return func(fr *frame, k *int) {
				v := fr.get(instr.X)
				fr.env[instr] = &mapIter{iter: reflect.ValueOf(v).MapRange()}
			}
		default:
			panic("unreachable")
		}
	case *ssa.Next:
		if instr.IsString {
			return func(fr *frame, k *int) {
				fr.env[instr] = fr.get(instr.Iter).(*stringIter).next()
			}
		} else {
			return func(fr *frame, k *int) {
				fr.env[instr] = fr.get(instr.Iter).(*mapIter).next()
			}
		}
	case *ssa.TypeAssert:
		typ := interp.preToType(instr.AssertedType)
		if fn, ok := instr.X.(*ssa.Function); ok {
			f := interp.makeFuncEx(nil, interp.preToType(fn.Type()), fn, nil).Interface()
			return func(fr *frame, k *int) {
				fr.env[instr] = typeAssert(interp, instr, typ, f)
			}
		} else {
			return func(fr *frame, k *int) {
				v := fr.get(instr.X)
				fr.env[instr] = typeAssert(interp, instr, typ, v)
			}
		}
	case *ssa.Extract:
		if *instr.Referrers() == nil {
			return nil
		}
		return func(fr *frame, k *int) {
			fr.env[instr] = fr.get(instr.Tuple).(tuple)[instr.Index]
		}
	// Instructions executed for effect
	case *ssa.Jump:
		return func(fr *frame, k *int) {
			fr.prevBlock, fr.block = fr.block, fr.block.Succs[0]
			*k = kJump
		}
	case *ssa.If:
		ic := pfn.getIndex(instr.Cond)
		return func(fr *frame, k *int) {
			succ := 1
			if v := fr.reg(ic); v.(bool) {
				succ = 0
			}
			fr.prevBlock, fr.block = fr.block, fr.block.Succs[succ]
			*k = kJump
		}
	case *ssa.Return:
		switch n := len(instr.Results); n {
		case 0:
			return func(fr *frame, k *int) {
				fr.block = nil
				*k = kReturn
			}
		case 1:
			ir := pfn.getIndex(instr.Results[0])
			return func(fr *frame, k *int) {
				//fr.result = fr.get(instr.Results[0])
				fr.result = fr.reg(ir)
				fr.block = nil
				*k = kReturn
				//log.Println("===>", fr.pfn.Fn.Name(), ir, fr.result, fr.reg(ir))
			}
		default:
			res := make([]value, n, n)
			return func(fr *frame, k *int) {
				for i := 0; i < n; i++ {
					res[i] = fr.get(instr.Results[i])
				}
				fr.result = tuple(res)
				fr.block = nil
				*k = kReturn
			}
		}
	case *ssa.RunDefers:
		return func(fr *frame, k *int) {
			fr.runDefers()
		}
	case *ssa.Panic:
		return func(fr *frame, k *int) {
			panic(targetPanic{fr.get(instr.X)})
		}
	case *ssa.Go:
		return func(fr *frame, k *int) {
			fn, args := interp.prepareCall(fr, &instr.Call)
			atomic.AddInt32(&interp.goroutines, 1)
			go func() {
				interp.call(nil, instr.Pos(), fn, args, instr.Call.Args)
				atomic.AddInt32(&interp.goroutines, -1)
			}()
		}
	case *ssa.Defer:
		return func(fr *frame, k *int) {
			fn, args := interp.prepareCall(fr, &instr.Call)
			fr.defers = &deferred{
				fn:      fn,
				args:    args,
				ssaArgs: instr.Call.Args,
				instr:   instr,
				tail:    fr.defers,
			}
		}
	case *ssa.Send:
		return func(fr *frame, k *int) {
			c := fr.get(instr.Chan)
			x := fr.get(instr.X)
			ch := reflect.ValueOf(c)
			if x == nil {
				ch.Send(reflect.New(ch.Type().Elem()).Elem())
			} else {
				ch.Send(reflect.ValueOf(x))
			}
		}
	case *ssa.Store:
		// skip struct field _
		if addr, ok := instr.Addr.(*ssa.FieldAddr); ok {
			if s, ok := addr.X.Type().(*types.Pointer).Elem().(*types.Struct); ok {
				if s.Field(addr.Field).Name() == "_" {
					return nil
				}
			}
		}
		ia := pfn.getIndex(instr.Addr)
		iv := pfn.getIndex(instr.Val)
		return func(fr *frame, k *int) {
			x := reflect.ValueOf(fr.reg(ia)) //fr.get(instr.Addr))
			val := fr.reg(iv)                //fr.get(instr.Val)
			switch fn := val.(type) {
			case *ssa.Function:
				f := interp.makeFunc(fr, interp.toType(fn.Type()), fn)
				SetValue(x.Elem(), f)
			default:
				v := reflect.ValueOf(val)
				if v.IsValid() {
					SetValue(x.Elem(), v)
				} else {
					SetValue(x.Elem(), reflect.New(x.Elem().Type()).Elem())
				}
			}
		}
	case *ssa.MapUpdate:
		return func(fr *frame, k *int) {
			vm := reflect.ValueOf(fr.get(instr.Map))
			vk := reflect.ValueOf(fr.get(instr.Key))
			v := fr.get(instr.Value)
			if fn, ok := v.(*ssa.Function); ok {
				typ := interp.toType(fn.Type())
				v = interp.makeFunc(fr, typ, fn).Interface()
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
		}

	case *ssa.DebugRef:
		if interp.fnDebug == nil {
			return nil
		} else {
			return func(fr *frame, k *int) {
				ref := &DebugInfo{DebugRef: instr, fset: interp.fset}
				ref.toValue = func() (*types.Var, interface{}, bool) {
					if v, ok := instr.Object().(*types.Var); ok {
						return v, fr.get(instr.X), true
					}
					return nil, nil, false
				}
				interp.fnDebug(ref)
			}
		}
	default:
		panic(fmt.Errorf("unreachable %T", instr))
	}
}

func makeCallInstr(interp *Interp, pfn *Function, instr ssa.Value, call ssa.CallCommon) func(fr *frame, k *int) {
	pos := instr.Pos()
	nargs := len(call.Args)
	targs := make([]interface{}, nargs, nargs)
	var fetch []int
	var fetchMode bool
	ii := pfn.getIndex(instr)
	iargs := make([]int, nargs, nargs)
	_ = iargs
	for i := 0; i < nargs; i++ {
		iargs[i] = pfn.getIndex(call.Args[i])
		switch arg := call.Args[i].(type) {
		case *ssa.Function:
			targs[i] = interp.makeFuncEx(nil, interp.preToType(arg.Type()), arg, nil).Interface()
			fetchMode = true
		case *constValue:
			targs[i] = arg.Value
			fetchMode = true
		case *ssa.Const:
			targs[i] = constToValue(interp, arg)
			fetchMode = true
		case *ssa.Global:
			if v, ok := globalToValue(interp, arg); ok {
				targs[i] = v
				fetchMode = true
			} else {
				panic(fmt.Errorf("not found global %v", arg))
			}
		default:
			fetch = append(fetch, i)
		}
	}
	if call.Method == nil {
		switch fn := call.Value.(type) {
		case *ssa.Function:
			if fn.Blocks == nil {
				ext, ok := findExternFunc(interp, fn)
				if !ok {
					// skip pkg.init
					if fn.Pkg != nil && fn.Name() == "init" && fn.Type().String() == "func()" {
						return nil
					}
					panic(fmt.Errorf("no code for function: %v", fn))
				}
				if nargs == 0 {
					return func(fr *frame, k *int) {
						fr.env[instr] = interp.callReflect(fr, pos, ext, nil, nil)
					}
				} else {
					if fetchMode {
						return func(fr *frame, k *int) {
							args := make([]value, nargs, nargs)
							copy(args, targs)
							for _, i := range fetch {
								args[i] = fr.env[call.Args[i]]
							}
							fr.env[instr] = interp.callReflect(fr, pos, ext, args, nil)
						}
					} else {
						return func(fr *frame, k *int) {
							args := make([]value, nargs, nargs)
							for i := 0; i < nargs; i++ {
								args[i] = fr.reg(iargs[i]) //fr.env[call.Args[i]]
							}
							fr.setReg(ii, interp.callReflect(fr, pos, ext, args, nil))
						}
					}
				}
			} else {
				if nargs == 0 {
					return func(fr *frame, k *int) {
						fr.env[instr] = interp.callFunction(fr, pos, fn, nil, nil)
					}
				} else {
					pfn := interp.funcs[fn]
					if fetchMode {
						return func(fr *frame, k *int) {
							args := make([]value, nargs, nargs)
							copy(args, targs)
							for _, i := range fetch {
								//args[i] = fr.env[call.Args[i]]
								args[i] = fr.reg(iargs[i])
							}
							// fr.env[instr] = interp.callFunction(fr, pos, fn, args, nil)
							fr.setReg(ii, interp.callFunctionEx(fr, pos, fn, pfn, args, nil))
						}
					} else {
						vargs := make([]value, nargs, nargs)
						return func(fr *frame, k *int) {
							args := append([]value{}, vargs...)
							for i := 0; i < nargs; i++ {
								//args[i] = fr.env[call.Args[i]]
								args[i] = fr.reg(iargs[i])
							}
							// fr.env[instr] = interp.callFunction(fr, pos, fn, args, nil)
							fr.setReg(ii, interp.callFunctionEx(fr, pos, fn, pfn, args, nil))
						}
					}
				}
			}
		case *ssa.Builtin:
			if nargs == 0 {
				return func(fr *frame, k *int) {
					fr.env[instr] = interp.callBuiltin(fr, pos, fn, nil, nil)
				}
			} else {
				if fetchMode {
					return func(fr *frame, k *int) {
						args := make([]value, nargs, nargs)
						copy(args, targs)
						for _, i := range fetch {
							args[i] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callBuiltin(fr, pos, fn, args, call.Args)
					}
				} else {
					return func(fr *frame, k *int) {
						args := make([]value, nargs, nargs)
						for i := 0; i < nargs; i++ {
							args[i] = fr.reg(iargs[i])
						}
						interp.callBuiltin(fr, pos, fn, args, call.Args)
					}
				}
			}
		case *ssa.MakeClosure:
			if nargs == 0 {
				return func(fr *frame, k *int) {
					var bindings []value
					for _, binding := range fn.Bindings {
						bindings = append(bindings, fr.get(binding))
					}
					fr.env[instr] = interp.callFunction(fr, pos, fn.Fn.(*ssa.Function), nil, bindings)
				}
			} else {
				if fetchMode {
					return func(fr *frame, k *int) {
						var bindings []value
						for _, binding := range fn.Bindings {
							bindings = append(bindings, fr.get(binding))
						}
						args := make([]value, nargs, nargs)
						copy(args, targs)
						for _, i := range fetch {
							args[i] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callFunction(fr, pos, fn.Fn.(*ssa.Function), args, bindings)
					}
				} else {
					return func(fr *frame, k *int) {
						var bindings []value
						for _, binding := range fn.Bindings {
							bindings = append(bindings, fr.get(binding))
						}
						args := make([]value, nargs, nargs)
						for i := 0; i < nargs; i++ {
							args[i] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callFunction(fr, pos, fn.Fn.(*ssa.Function), args, bindings)
					}
				}
			}
		default:
			typ := interp.preToType(call.Value.Type())
			if typ.Kind() != reflect.Func {
				panic("unsupport")
			}
			if nargs == 0 {
				return func(fr *frame, k *int) {
					fn := fr.get(call.Value)
					switch fn := fn.(type) {
					case *ssa.Function:
						fr.env[instr] = interp.callFunction(fr, pos, fn, nil, nil)
					case *closure:
						fr.env[instr] = interp.callFunction(fr, pos, fn.Fn, nil, fn.Env)
					case *ssa.Builtin:
						fr.env[instr] = interp.callBuiltin(fr, pos, fn, nil, nil)
					default:
						fr.env[instr] = interp.callReflect(fr, pos, reflect.ValueOf(fn), nil, nil)
					}
				}
			} else {
				if fetchMode {
					return func(fr *frame, k *int) {
						fn := fr.get(call.Value)
						args := make([]value, nargs, nargs)
						copy(args, targs)
						for _, i := range fetch {
							args[i] = fr.env[call.Args[i]]
						}
						switch fn := fn.(type) {
						case *ssa.Function:
							fr.env[instr] = interp.callFunction(fr, pos, fn, args, nil)
						case *closure:
							fr.env[instr] = interp.callFunction(fr, pos, fn.Fn, args, fn.Env)
						case *ssa.Builtin:
							fr.env[instr] = interp.callBuiltin(fr, pos, fn, args, call.Args)
						default:
							fr.env[instr] = interp.callReflect(fr, pos, reflect.ValueOf(fn), args, nil)
						}
					}
				} else {
					return func(fr *frame, k *int) {
						fn := fr.get(call.Value)
						args := make([]value, nargs, nargs)
						for i := 0; i < nargs; i++ {
							args[i] = fr.env[call.Args[i]]
						}
						switch fn := fn.(type) {
						case *ssa.Function:
							fr.env[instr] = interp.callFunction(fr, pos, fn, args, nil)
						case *closure:
							fr.env[instr] = interp.callFunction(fr, pos, fn.Fn, args, fn.Env)
						case *ssa.Builtin:
							fr.env[instr] = interp.callBuiltin(fr, pos, fn, args, call.Args)
						default:
							fr.env[instr] = interp.callReflect(fr, pos, reflect.ValueOf(fn), args, nil)
						}
					}
				}
			}
		}
	} else {
		margs := nargs + 1
		var isReflect bool
		if pkg := call.Method.Pkg(); pkg == nil {
			// error.Error
			isReflect = true
		} else if _, found := interp.installed(pkg.Path()); found {
			isReflect = true
		}
		if isReflect {
			mname := call.Method.Name()
			// skip unexport method
			if !token.IsExported(mname) {
				return nil
			}
			switch fullName := call.Method.FullName(); fullName {
			case "(reflect.Type).Method", "(reflect.Type).MethodByName":
				var ext reflect.Value
				if mname == "Method" {
					ext = reflect.ValueOf(reflectx.MethodByIndex)
				} else {
					ext = reflect.ValueOf(reflectx.MethodByName)
				}
				if fetchMode {
					return func(fr *frame, k *int) {
						args := make([]value, margs, margs)
						args[0] = fr.get(call.Value)
						copy(args[1:], targs)
						for _, i := range fetch {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callReflect(fr, pos, ext, args, nil)
					}
				} else {
					return func(fr *frame, k *int) {
						args := make([]value, margs, margs)
						args[0] = fr.get(call.Value)
						for i := 0; i < nargs; i++ {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callReflect(fr, pos, ext, args, nil)
					}
				}
			default:
				if fetchMode {
					return func(fr *frame, k *int) {
						var ext reflect.Value
						v := fr.get(call.Value)
						rtype := reflect.TypeOf(v)
						if f, ok := reflectx.MethodByName(rtype, mname); ok {
							ext = f.Func
						} else {
							panic(runtimeError("invalid memory address or nil pointer dereference"))
						}
						args := make([]value, margs, margs)
						args[0] = v
						copy(args[1:], targs)
						for _, i := range fetch {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callReflect(fr, instr.Pos(), ext, args, nil)
					}
				} else {
					return func(fr *frame, k *int) {
						var ext reflect.Value
						v := fr.get(call.Value)
						rtype := reflect.TypeOf(v)
						if f, ok := reflectx.MethodByName(rtype, mname); ok {
							ext = f.Func
						} else {
							panic(runtimeError("invalid memory address or nil pointer dereference"))
						}
						args := make([]value, margs, margs)
						args[0] = v
						for i := 0; i < nargs; i++ {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callReflect(fr, instr.Pos(), ext, args, nil)
					}
				}
			}
		} else {
			if fetchMode {
				return func(fr *frame, k *int) {
					v := fr.get(call.Value)
					rtype := reflect.TypeOf(v)
					if t, ok := interp.findType(rtype, true); ok {
						fn := lookupMethod(interp, t, call.Method)
						if fn == nil {
							// Unreachable in well-typed programs.
							panic(fmt.Sprintf("method set for dynamic type %v does not contain %s", t, call.Method))
						}
						args := make([]value, margs, margs)
						args[0] = v
						copy(args[1:], targs)
						for _, i := range fetch {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callFunction(fr, pos, fn, args, nil)
					} else {
						var ext reflect.Value
						mname := call.Method.Name()
						if s := rtype.String(); (s == "*reflect.rtype" || s == "reflect.Type") &&
							mname == "Method" || mname == "MethodByName" {
							if mname == "Method" {
								ext = reflect.ValueOf(reflectx.MethodByIndex)
							} else {
								ext = reflect.ValueOf(reflectx.MethodByName)
							}
						} else {
							if f, ok := reflectx.MethodByName(rtype, mname); ok {
								ext = f.Func
							} else {
								panic(runtimeError("invalid memory address or nil pointer dereference"))
							}
						}
						args := make([]value, margs, margs)
						args[0] = v
						copy(args[1:], targs)
						for _, i := range fetch {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callReflect(fr, pos, ext, args, nil)
					}
				}
			} else {
				return func(fr *frame, k *int) {
					v := fr.get(call.Value)
					rtype := reflect.TypeOf(v)
					if t, ok := interp.findType(rtype, true); ok {
						fn := lookupMethod(interp, t, call.Method)
						if fn == nil {
							// Unreachable in well-typed programs.
							panic(fmt.Sprintf("method set for dynamic type %v does not contain %s", t, call.Method))
						}
						args := make([]value, margs, margs)
						args[0] = v
						for i := 0; i < nargs; i++ {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callFunction(fr, pos, fn, args, nil)
					} else {
						var ext reflect.Value
						mname := call.Method.Name()
						if s := rtype.String(); (s == "*reflect.rtype" || s == "reflect.Type") &&
							mname == "Method" || mname == "MethodByName" {
							if mname == "Method" {
								ext = reflect.ValueOf(reflectx.MethodByIndex)
							} else {
								ext = reflect.ValueOf(reflectx.MethodByName)
							}
						} else {
							if f, ok := reflectx.MethodByName(rtype, mname); ok {
								ext = f.Func
							} else {
								panic(runtimeError("invalid memory address or nil pointer dereference"))
							}
						}
						args := make([]value, margs, margs)
						args[0] = v
						for i := 0; i < nargs; i++ {
							args[i+1] = fr.env[call.Args[i]]
						}
						fr.env[instr] = interp.callReflect(fr, pos, ext, args, nil)
					}
				}
			}
		}
	}
}
