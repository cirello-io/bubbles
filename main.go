package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os/exec"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
)

type dep struct {
	Left, Right string
}

type graph struct {
	PID    string
	Name   string
	Input  []dep
	Output template.HTML
	Err    string
	Src    string
}

type route struct {
	method string
	path   string
}

type bubbleState string

const (
	initial bubbleState = "initial"
	started bubbleState = "started"
	done    bubbleState = "done"
	aborted bubbleState = "aborted"
)

func (b bubbleState) color() string {
	switch b {
	case started:
		return "style=filled,fillcolor=yellow"
	case done:
		return "style=filled,fillcolor=lightgreen"
	case aborted:
		return "style=filled,fillcolor=red"
	default:
		return ""
	}
}

type bubble struct {
	Bubble string
	State  bubbleState
}

type project struct {
	ID   uint64
	Name string
}

func main() {
	log.SetPrefix("bubbleproject: ")
	log.SetFlags(0)

	var dbMu sync.Mutex
	db, err := sql.Open("sqlite3", "state.db")
	check(err)
	defer db.Close()

	sqlStmt := `
	create table if not exists pairs (project bigint, left text, right text);
	create table if not exists bubbles (project bigint, bubble text, state text);
	create unique index if not exists bubbles_project_bubble ON bubbles (project, bubble);
	create table if not exists projects (project integer primary key autoincrement, name text);
	`
	_, err = db.Exec(sqlStmt)
	check(err)

	indexHTMLTpl := template.Must(template.New("index.html").Parse(indexHTMLTpl))
	projectTpl := template.Must(template.New("index.html").Parse(projectTpl))

	http.HandleFunc("/flip", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		pID := r.URL.Query().Get("pID")
		if _, err := db.Exec(`
			insert into bubbles (project, bubble, state) values (?, ?, 'started')
				on conflict (project, bubble) do update set
				state = case
				when state = '' then 'initial'
				when state = 'initial' then 'started'
				when state = 'started' then 'done'
				when state = 'done' then 'aborted'
				when state = 'aborted' then 'initial'
				else 'initial'
				end
		`, pID, r.URL.Query().Get("bubble")); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/projects?pID=%v", pID), http.StatusSeeOther)
	})

	http.HandleFunc("/remove", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		row := db.QueryRow("delete from pairs where left = ? and right = ? returning project", r.URL.Query().Get("left"), r.URL.Query().Get("right"))
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		var pID uint64
		if err := row.Scan(&pID); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/projects?pID=%v", pID), http.StatusSeeOther)
	})

	http.HandleFunc("/store", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		r.ParseForm()
		tx, err := db.Begin()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		stmt, err := tx.Prepare("insert into pairs (project, left, right) values(?, ?, ?)")
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		pID := r.URL.Query().Get("pID")
		newLeft := strings.TrimSpace(r.PostForm.Get("newLeft"))
		newRight := strings.TrimSpace(r.PostForm.Get("newRight"))
		if pID != "" && newLeft != "" && newRight != "" {
			if _, err := stmt.Exec(pID, newLeft, newRight); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/projects?pID=%v", pID), http.StatusSeeOther)
	})

	http.HandleFunc("/projects/new", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		name := r.FormValue("name")
		dbMu.Lock()
		defer dbMu.Unlock()
		result, err := db.Exec(`insert into projects (name) values (?)`, name)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		pID, err := result.LastInsertId()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/projects?pID=%v", uint64(pID)), http.StatusSeeOther)
	})
	http.HandleFunc("/projects", func(w http.ResponseWriter, r *http.Request) {
		pID := r.URL.Query().Get("pID")
		r.ParseForm()
		var deps []dep
		dbMu.Lock()
		defer dbMu.Unlock()
		var projectName string
		rowProject := db.QueryRow("select name from projects where project = ?", pID)
		if err := rowProject.Scan(&projectName); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		rowsPairs, err := db.Query("select left, right from pairs where project = ?", pID)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		defer rowsPairs.Close()
		input := &bytes.Buffer{}
		knownBubblesIdx := make(map[string]struct{})
		fmt.Fprintln(input, "digraph G {")
		for rowsPairs.Next() {
			var dep dep
			if err := rowsPairs.Scan(&dep.Left, &dep.Right); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
			knownBubblesIdx[dep.Left] = struct{}{}
			knownBubblesIdx[dep.Right] = struct{}{}
			fmt.Fprintf(input, "	%q -> %q\n", dep.Left, dep.Right)
			deps = append(deps, dep)
		}
		if err := rowsPairs.Err(); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		rowsBubbles, err := db.Query("select bubble, state from bubbles where project = ?", pID)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		defer rowsBubbles.Close()
		for rowsBubbles.Next() {
			var bubble bubble
			if err := rowsBubbles.Scan(&bubble.Bubble, &bubble.State); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
			if _, ok := knownBubblesIdx[bubble.Bubble]; !ok {
				continue
			}
			delete(knownBubblesIdx, bubble.Bubble)
			fmt.Fprintf(input, `	%q [href="/flip?pID=%v&bubble=%v",%v]`, bubble.Bubble, pID, template.URLQueryEscaper(bubble.Bubble), bubble.State.color())
			fmt.Fprintln(input)
		}
		if err := rowsBubbles.Err(); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		var knownBubbles []string
		for k := range knownBubblesIdx {
			knownBubbles = append(knownBubbles, k)
		}
		sort.Strings(knownBubbles)
		for _, bubble := range knownBubbles {
			fmt.Fprintf(input, `	%q [href="/flip?pID=%v&bubble=%v"]`, bubble, pID, template.URLQueryEscaper(bubble))
			fmt.Fprintln(input)
		}
		fmt.Fprintln(input, "}")
		src := input.String()
		cmd := exec.CommandContext(r.Context(), "dot", "-Tsvg")
		cmd.Stdin = input
		var outBuf bytes.Buffer
		cmd.Stdout = &outBuf
		var errBuf bytes.Buffer
		cmd.Stderr = &outBuf
		if err := cmd.Run(); err != nil {
			errBuf.WriteString("\n")
			errBuf.WriteString(err.Error())
		}
		projectTpl.Execute(w, graph{
			pID,
			projectName,
			deps,
			template.HTML(outBuf.String()),
			errBuf.String(),
			src,
		})
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rt := route{r.Method, r.URL.Path}
		switch rt {
		case route{http.MethodGet, "/"}:

			dbMu.Lock()
			defer dbMu.Unlock()
			rows, err := db.Query("select project, name from projects", r.URL.Query().Get("left"), r.URL.Query().Get("right"))
			if err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
			defer rows.Close()
			var projects []project
			for rows.Next() {
				var project project
				if err := rows.Scan(&project.ID, &project.Name); err != nil {
					http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
					return
				}
				projects = append(projects, project)
			}
			if err := rows.Err(); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
			indexHTMLTpl.Execute(w, struct {
				Project []project
			}{projects})
		default:
			fmt.Println(rt)
			http.NotFound(w, r)
			return
		}
	})
	check(http.ListenAndServe(":5466", nil))
}

func check(err error) {
	if err != nil {
		debug.PrintStack()
		log.Fatal(err)
	}
}

const indexHTMLTpl = `
<html>
	<body>
	<h1>Projects</h1>
	<ul>
	{{ range .Project }}
		<il><a href="/projects?pID={{.ID}}">{{.Name}}</a></il>
	</ul>
	{{ end }}
	<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new">
		<label>project name:<input type="text" name="name"></label>
		<input type="submit">
	</form>
	</body>
</html>
`

const projectTpl = `
<html>
	<body>
		<a href="/">&lt;&lt; main menu</a>
		<h1>Project: {{ .Name }}</h1>
		<form method="POST" enctype="application/x-www-form-urlencoded" action="/store?pID={{ .PID }}">
			{{ if .Err }}
			<div>{{ .Err }}</div>
			{{ end }}
			<div>
				<table border=1>
					<thead>
						<th colspan=2>... must happen before ...</th>
					</thead>
					{{ range .Input }}
					<tr>
						<td>{{ .Left }}</td>
						<td>{{ .Right }}</td>
						<td><a href="/remove?left={{.Left}}&right={{.Right}}">❌</a></td>
					</tr>
					{{ end }}
					<tr>
						<td><input type="text" name="newLeft"></td>
						<td><input type="text" name="newRight"></td>
						<td></td>
					</tr>
				</table>
			</div>
			<div><svg width="50%" height="50%">
				{{ .Output }}
			</svg></div>
			<input type="submit" onClick="javascript: (function(){document.forms[0].submit()})()" value="save"/>
		</form>

		<details>
			<summary>source</summary>
			<pre>
{{ .Src }}
			</pre>
		</details>
	</body>
</html>
`
