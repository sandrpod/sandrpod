// Copyright 2026 SandrPod Contributors
//
// The API server's entry point. Everything it needs lives in pkg/server, so
// that another program (a commercial build, a test harness) can construct the
// same handler from a config and a set of stores instead of shelling out to
// this binary.
package main

import "github.com/sandrpod/sandrpod/pkg/server"

func main() { server.Main() }
