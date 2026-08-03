package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func TestCallTarStreamWriteSignatures(t *testing.T) {
	ctx := context.Background()
	src := sparse.Dense(bytes.NewReader(nil), 0)
	oldWrite := func(context.Context, io.Writer, string, sparse.Source) (string, error) {
		return "sha256:digest", nil
	}
	type option string
	newWrite := func(context.Context, io.Writer, string, sparse.Source, ...option) (string, string, error) {
		return "hmac", "digest", nil
	}
	for _, fn := range []any{oldWrite, newWrite} {
		if err := callTarStreamWrite(fn, ctx, io.Discard, "image", src); err != nil {
			t.Fatal(err)
		}
	}

	wantErr := errors.New("write failed")
	failing := func(context.Context, io.Writer, string, sparse.Source) (string, error) {
		return "", wantErr
	}
	if err := callTarStreamWrite(failing, ctx, io.Discard, "image", src); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if err := callTarStreamWrite(func() {}, ctx, io.Discard, "image", src); err == nil {
		t.Fatal("invalid WriteTo signature accepted")
	}
	hybridOldInputs := func(context.Context, io.Writer, string, sparse.Source) (string, string, error) {
		return "sha256", "digest", nil
	}
	hybridFinalInputs := func(context.Context, io.Writer, string, sparse.Source, ...option) (string, error) {
		return "sha256:digest", nil
	}
	for _, fn := range []any{hybridOldInputs, hybridFinalInputs} {
		if err := callTarStreamWrite(fn, ctx, io.Discard, "image", src); err == nil {
			t.Fatal("hybrid WriteTo signature accepted")
		}
	}
}

func TestWriteTarStreamUsesCurrentAccelerator(t *testing.T) {
	var output bytes.Buffer
	if err := writeTarStream(context.Background(), &output, "image", sparse.Dense(bytes.NewReader(nil), 0)); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		t.Fatal("WriteTo emitted no artifact")
	}
}
