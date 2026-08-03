package main

import (
	"context"
	"fmt"
	"io"
	"reflect"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// writeTarStream is a short-lived source-compatibility bridge across
// accelerator #27's WriteTo digest API change. It preserves the current
// plaintext call (no options) and only normalizes the ignored return values.
// Remove it once accelerator #27 is on main everywhere.
func writeTarStream(ctx context.Context, w io.Writer, name string, src sparse.Source) error {
	return callTarStreamWrite(tarstream.WriteTo, ctx, w, name, src)
}

func callTarStreamWrite(fn any, ctx context.Context, w io.Writer, name string, src sparse.Source) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tarstream transition: invoke WriteTo: %v", recovered)
		}
	}()

	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		return fmt.Errorf("tarstream transition: WriteTo is %T, not a function", fn)
	}
	t := v.Type()
	if t.NumIn() != 4 && !(t.NumIn() == 5 && t.IsVariadic()) {
		return fmt.Errorf("tarstream transition: unsupported WriteTo input signature %s", t)
	}
	args := []reflect.Value{
		reflect.ValueOf(ctx),
		reflect.ValueOf(w),
		reflect.ValueOf(name),
		reflect.ValueOf(src),
	}
	for i := range args {
		if !args[i].IsValid() || !args[i].Type().AssignableTo(t.In(i)) {
			return fmt.Errorf("tarstream transition: WriteTo argument %d has type %T, want %s", i, args[i].Interface(), t.In(i))
		}
	}

	results := v.Call(args)
	var errorIndex int
	switch len(results) {
	case 2:
		if results[0].Kind() != reflect.String {
			return fmt.Errorf("tarstream transition: legacy digest result is %s", results[0].Type())
		}
		errorIndex = 1
	case 3:
		if results[0].Kind() != reflect.String || results[1].Kind() != reflect.String {
			return fmt.Errorf("tarstream transition: final digest results are %s and %s", results[0].Type(), results[1].Type())
		}
		errorIndex = 2
	default:
		return fmt.Errorf("tarstream transition: unsupported WriteTo result count %d", len(results))
	}
	return transitionResultError(results[errorIndex])
}

func transitionResultError(value reflect.Value) error {
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if !value.IsValid() || !value.Type().Implements(errorType) {
		return fmt.Errorf("tarstream transition: error result has type %s", value.Type())
	}
	if value.IsNil() {
		return nil
	}
	return value.Interface().(error)
}
