package main

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var templatesFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"formatTime": func(layout string, t any) string {
		if v, ok := t.(interface{ Format(string) string }); ok {
			return v.Format(layout)
		}
		return ""
	},
}).ParseFS(templatesFS, "templates/*.html"))
