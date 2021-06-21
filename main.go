package main

import (
	"bytes"
	"log"
	"net/http"
	"os/exec"
	"runtime/debug"
	"strings"
	"text/template"
)

func main() {
	log.SetPrefix("bubbleproject: ")
	log.SetFlags(0)
	indexHtmlTpl := template.Must(template.New("index.html").Parse(indexHtmlTpl))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		input := r.PostFormValue("in")
		cmd := exec.CommandContext(r.Context(), "dot", "-Tsvg")
		cmd.Stdin = strings.NewReader(input)
		var outBuf bytes.Buffer
		cmd.Stdout = &outBuf
		var errBuf bytes.Buffer
		cmd.Stderr = &outBuf
		cmd.Run()
		indexHtmlTpl.Execute(w, struct {
			Input  string
			Output string
			Err    string
		}{
			input,
			outBuf.String(),
			errBuf.String(),
		})
	})
	check(http.ListenAndServe(":5466", nil))
}

func check(err error) {
	if err != nil {
		debug.PrintStack()
		log.Fatal(err)
	}
}

const indexHtmlTpl = `
<html>
	<head>
		<script src="https://ajax.googleapis.com/ajax/libs/jquery/3.6.0/jquery.min.js"></script>
		<!-- CSS only -->
		<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/css/bootstrap.min.css" rel="stylesheet" integrity="sha384-+0n0xVW2eSR5OomGNYDnhzAbDsOXxcvSN1TPprVMTNDbiYZCxYbOOl7+AMvyTG2x" crossorigin="anonymous">
		<script src="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/js/bootstrap.bundle.min.js" integrity="sha384-gtEjrD/SeCtmISkJkNUaaKMoLD0//ElJ19smozuHV6z3Iehds+3Ulb9Bn9Plx0x4" crossorigin="anonymous"></script>
	</head>
	<body>
		<form method="POST" enctype="multipart/form-data" action="/">
			<div> <textarea name="in" width="50%" height="25%">{{.Input}}</textarea></div>
			<div> <svg width="50%" height="50%">
				{{ .Output }}
			</svg></div>
			{{ if .Err }}
			<div>{{ .Err }}</div>
			{{ end }}
			<button onClick="javascript: (function(){document.forms[0].submit()})()">save</button>
		</form>
	</body>
</html>
`
