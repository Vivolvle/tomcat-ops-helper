package helpergui

import _ "embed"

// indexHTML is the whole customer-facing UI: one self-contained page,
// vanilla JS, Chinese labels. No build chain, no external assets —
// everything ships inside helper-gui.exe.
//
//go:embed static/index.html
var indexHTML []byte
