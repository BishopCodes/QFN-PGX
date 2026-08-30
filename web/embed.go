// Package web holds the embedded console assets: one HTML file, one vanilla
// JS file, one stylesheet — go:embed ships them inside the single static
// binary, no build step, no Node on the Spark.
package web

import "embed"

//go:embed index.html app.js style.css login.js
var FS embed.FS
