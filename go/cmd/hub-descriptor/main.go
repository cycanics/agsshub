// hub-descriptor checks a description on stdin and writes the stored form on stdout: hub-topic's way to
// go/descriptor (docs/help.md). Exit 0, or 2 with the reason on stderr.
//
//	hub-descriptor [--topic] [--see <pattern>]... < descriptor.json
package main

import (
	"fmt"
	"io"
	"os"

	"agsshub/descriptor"
)

func refuse(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}

func main() {
	var o descriptor.Options
	a := os.Args[1:]
	for len(a) > 0 {
		switch a[0] {
		case "--topic":
			o.Topic = true
			a = a[1:]
		case "--see":
			if len(a) < 2 {
				refuse("description refused: --see needs a value")
			}
			o.See = append(o.See, a[1])
			a = a[2:]
		default:
			refuse("description refused: unexpected arg %q", a[0])
		}
	}
	in, err := io.ReadAll(io.LimitReader(os.Stdin, descriptor.MaxBytes+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub-descriptor: %v\n", err)
		os.Exit(1)
	}
	st, err := descriptor.Build(in, o)
	if err != nil {
		refuse("description refused: %v", err)
	}
	if _, err := os.Stdout.Write(st.Bytes()); err != nil {
		os.Exit(1)
	}
}
