package main

import (
	"html/template"
	"path"
)

var templates map[string]*template.Template

func initTemplates() {
	fmap := template.FuncMap{"imageURL": imageURL}
	templates = map[string]*template.Template{
		"layout":   template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("index.html"), getTemplPath("posts.html"), getTemplPath("post.html"))),
		"user":     template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("user.html"), getTemplPath("posts.html"), getTemplPath("post.html"))),
		"posts":    template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(getTemplPath("posts.html"), getTemplPath("post.html"))),
		"post_id":  template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(getTemplPath("layout.html"), getTemplPath("post_id.html"), getTemplPath("post.html"))),
		"login":    template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("login.html"))),
		"register": template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("register.html"))),
		"banned":   template.Must(template.ParseFiles(getTemplPath("layout.html"), getTemplPath("banned.html"))),
	}
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}
