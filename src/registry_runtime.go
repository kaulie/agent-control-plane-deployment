package main

import "runtime"

func goosRuntime() string   { return runtime.GOOS }
func goarchRuntime() string { return runtime.GOARCH }
