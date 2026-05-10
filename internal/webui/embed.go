package webui

import "embed"

// embeddedViews holds the html/template files. //go:embed
// directives must be declared in the same package as the
// embedded directory, hence this file.
//
//go:embed views
var embeddedViews embed.FS

// embeddedStatic holds CSS / JS / icons. Same package
// constraint as embeddedViews.
//
//go:embed static
var embeddedStatic embed.FS
