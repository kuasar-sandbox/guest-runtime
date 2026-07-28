package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/guest-runtime/internal/runtimebundle"
)

func main() {
	input := flag.String("input", "", "raw EROFS input path")
	output := flag.String("output", "", "runtime bundle output path")
	flag.Parse()
	if *input == "" || *output == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: runtime-bundle -input RAW.EROFS -output RUNTIME.BUNDLE")
		os.Exit(2)
	}
	digest, err := runtimebundle.Build(*input, *output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runtime-bundle: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(digest)
}
