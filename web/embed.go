// Package web holds the embedded console assets: one HTML file, one vanilla
// JS, one stylesheet (Tailwind-built via `make webcss`), a vendored chart
// library — go:embed ships them inside the single static binary, no build
// step, no Node on the Spark.
package web

import "embed"

//go:embed index.html app.js style.css login.js vendor
var FS embed.FS
