package main

// ui.go — ฝังหน้า UI (templates/*.html) และไฟล์ static เข้าไปในไบนารีด้วย go:embed
// ผลคือ build ครั้งเดียวได้ไฟล์เดียว รันที่ไหนก็ไม่ต้องก๊อปโฟลเดอร์ templates/ static/ ตามไปด้วย

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticEmbed embed.FS

func loadTemplates() *template.Template {
	return template.Must(template.ParseFS(templatesFS, "templates/*.html"))
}

func staticFS() http.FileSystem {
	sub, err := fs.Sub(staticEmbed, "static")
	if err != nil {
		log.Fatal(err)
	}
	return http.FS(sub)
}
