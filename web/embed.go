package web

import "embed"

//go:embed index.html style.css app.js
var StaticFS embed.FS
