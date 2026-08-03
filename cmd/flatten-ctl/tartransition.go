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
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	stringType := reflect.TypeOf("")
	contextType := reflect.TypeOf((*context.Context)(nil)).Elem()
	writerType := reflect.TypeOf((*io.Writer)(nil)).Elem()
	sourceType := reflect.TypeOf((*sparse.Source)(nil)).Elem()
	fixedInputs := t.NumIn() >= 4 && t.In(0) == contextType && t.In(1) == writerType &&
		t.In(2) == stringType && t.In(3) == sourceType
	legacySignature := fixedInputs && t.NumIn() == 4 && !t.IsVariadic() && t.NumOut() == 2 &&
		t.Out(0) == stringType && t.Out(1) == errorType
	finalOption := t.NumIn() == 5 && t.IsVariadic() && t.In(4).Kind() == reflect.Slice &&
		t.In(4).Elem().PkgPath() == "github.com/kuasar-sandbox/accelerator/pkg/tarstream" &&
		t.In(4).Elem().Name() == "WriteOption"
	finalSignature := fixedInputs && finalOption && t.NumOut() == 3 &&
		t.Out(0) == stringType && t.Out(1) == stringType && t.Out(2) == errorType
	if !legacySignature && !finalSignature {
		return fmt.Errorf("tarstream transition: unsupported WriteTo signature %s", t)
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
	errorIndex := 1
	if finalSignature {
		errorIndex = 2
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
