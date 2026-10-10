package postgres

import (
	"bytes"
	_ "embed"
	"text/template"
)

//go:embed sql/migrations.sql.tmpl
var migrationsRaw string

var migrationsTmpl = template.Must(template.New("migrations").Parse(migrationsRaw))

func renderTemplate(tmpl *template.Template, data any) string {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		panic("render template: " + err.Error())
	}
	return buf.String()
}

// notifySQL creates the tables behind Notifier and CacheStore. It is a plain
// script, run after the templated migrations.
//
//go:embed sql/notify.sql
var notifySQL string

// memoryRaw creates the memory_record table behind agent/memory/pgstore. It
// is a template for the embedding dimension, run after notifySQL.
//
//go:embed sql/memory.sql.tmpl
var memoryRaw string

var memoryTmpl = template.Must(template.New("memory").Parse(memoryRaw))
