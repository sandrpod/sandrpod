// Copyright 2024 SandrPod
// A dependency-free single-page web console served at /console. It talks to the
// same REST API as the CLI/SDK using a token the operator pastes in (kept only
// in the browser tab's memory). Intentionally minimal — one embedded HTML file,
// no build step, no framework.

package main

import (
	_ "embed"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

func consoleHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}
