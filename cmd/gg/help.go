package main

import (
	"fmt"
	"io"
	"text/template"
)

// TODO
//
// 1. We do not currently catch/support/whatever a package's location on disk changing mid
//    run. For example if we initially call load and package p1 is at location l1, then we
//    call load again some time later and it has moved to l2.

func mainUsage(f io.Writer) {
	t := template.Must(template.New("").Parse(mainHelpTemplate))
	if err := t.Execute(f, nil); err != nil {
		fmt.Fprintf(f, "cannot write usage output: %v", err)
	}
}

var mainHelpTemplate = `
The gg command efficiently wraps go generate

TODO
====
Flesh out the following points:

* In module mode, only works for patterns in the main module
* Document the approach for declaring tool dependencies
* Relies on the tools.go precedent for a tools build tag

`[1:]
