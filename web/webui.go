// Package webui carries the viewer's built CSS and JS. Bun builds web/src
// into web/dist (make web.build); a binary built with -tags embed_web embeds
// that directory here, and one built without serves web/dist from disk in a
// checkout or falls back to a minimal stylesheet.
package webui

import "io/fs"

// Dist is the built asset tree, or nil when this build did not embed it.
var Dist fs.FS
