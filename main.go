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
	PID     string
	Name    string
	Input   []dep
	Output  template.HTML
	Err     string
	Src     string
	Details string
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
	create table if not exists details (project integer primary key, details longtext);
	`
	_, err = db.Exec(sqlStmt)
	check(err)
	conditionalMigrations := [...]struct {
		test      string
		migration string
	}{
		{`select freeform from projects`, `alter table projects add column freeform bool not null default false;`},
	}
	for _, m := range conditionalMigrations {
		if _, err := db.Exec(m.test); err != nil {
			_, err = db.Exec(m.migration)
			check(err)
		}
	}

	indexHTMLTpl := template.Must(template.New("index.html").Parse(indexHTMLTpl))
	projectTpl := template.Must(template.New("index.html").Parse(projectTpl))
	freeformProjectTpl := template.Must(template.New("index.html").Parse(freeformProjectTpl))

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

	http.HandleFunc("/rename", func(w http.ResponseWriter, r *http.Request) {
		pID := r.URL.Query().Get("pID")
		dbMu.Lock()
		defer dbMu.Unlock()
		r.ParseForm()
		tx, err := db.Begin()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec("update pairs set left = ? where project = ? and left = ?", r.PostForm.Get("to"), pID, r.PostForm.Get("from")); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec("update pairs set right = ? where project = ? and right = ?", r.PostForm.Get("to"), pID, r.PostForm.Get("from")); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec("update bubbles set bubble = ? where project = ? and bubble = ?", r.PostForm.Get("to"), pID, r.PostForm.Get("from")); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
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

	http.HandleFunc("/details", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		r.ParseForm()
		tx, err := db.Begin()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		stmt, err := tx.Prepare("insert into details (project, details) values (?, ?) ON CONFLICT (project) DO UPDATE SET details = excluded.details")
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		pID := r.URL.Query().Get("pID")
		details := strings.TrimSpace(r.PostForm.Get("details"))
		if _, err := stmt.Exec(pID, details); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}

		http.Redirect(w, r, fmt.Sprintf("/projects?pID=%v", pID), http.StatusSeeOther)
	})

	http.HandleFunc("/projects/new/freeform", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		name := r.FormValue("name")
		dbMu.Lock()
		defer dbMu.Unlock()
		result, err := db.Exec(`insert into projects (name,freeform) values (?,?)`, name, true)
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
		var (
			projectName string
			freeform    bool
		)
		rowProject := db.QueryRow("select name, freeform from projects where project = ?", pID)
		if err := rowProject.Scan(&projectName, &freeform); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		switch freeform {
		case true:
			var projectDetails string
			rowDetails := db.QueryRow("select details from details where project = ?", pID)
			if err := rowDetails.Scan(&projectDetails); err != nil && err != sql.ErrNoRows {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
			src := projectDetails
			cmd := exec.CommandContext(r.Context(), "dot", "-Tsvg")
			cmd.Stdin = strings.NewReader(projectDetails)
			var outBuf bytes.Buffer
			cmd.Stdout = &outBuf
			var errBuf bytes.Buffer
			cmd.Stderr = &outBuf
			if err := cmd.Run(); err != nil {
				errBuf.WriteString("\n")
				errBuf.WriteString(err.Error())
			}
			freeformProjectTpl.Execute(w, graph{
				PID:     pID,
				Name:    projectName,
				Input:   deps,
				Output:  template.HTML(outBuf.String()),
				Err:     errBuf.String(),
				Src:     src,
				Details: projectDetails,
			})
			return
		case false:
			var projectDetails string
			rowDetails := db.QueryRow("select details from details where project = ?", pID)
			if err := rowDetails.Scan(&projectDetails); err != nil && err != sql.ErrNoRows {
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
			sort.Slice(deps, func(a, b int) bool {
				if cmp := strings.Compare(deps[a].Left, deps[b].Left); cmp != 0 {
					return cmp < 0
				}
				if cmp := strings.Compare(deps[a].Right, deps[b].Right); cmp != 0 {
					return cmp < 0
				}
				return false
			})
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
				PID:     pID,
				Name:    projectName,
				Input:   deps,
				Output:  template.HTML(outBuf.String()),
				Err:     errBuf.String(),
				Src:     src,
				Details: projectDetails,
			})
		}
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
<!doctype html>
<html lang="en">
	<head>
		<meta charset="utf-8">
		<meta name="viewport" content="width=device-width, initial-scale=1">
		<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/css/bootstrap.min.css" rel="stylesheet" integrity="sha384-+0n0xVW2eSR5OomGNYDnhzAbDsOXxcvSN1TPprVMTNDbiYZCxYbOOl7+AMvyTG2x" crossorigin="anonymous">
		<script src="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/js/bootstrap.bundle.min.js" integrity="sha384-gtEjrD/SeCtmISkJkNUaaKMoLD0//ElJ19smozuHV6z3Iehds+3Ulb9Bn9Plx0x4" crossorigin="anonymous"></script>
	</head>
	<body>
		<nav class="navbar navbar-expand-lg navbar-light bg-light">
			<div class="container-fluid">
				<a class="navbar-brand" href="/">Bubbles</a>
				<button class="navbar-toggler" type="button" data-bs-toggle="collapse" data-bs-target="#navbarSupportedContent" aria-controls="navbarSupportedContent" aria-expanded="false" aria-label="Toggle navigation">
					<span class="navbar-toggler-icon"></span>
				</button>

				<div class="collapse navbar-collapse" id="navbarSupportedContent">
					<ul class="navbar-nav me-auto mb-2 mb-lg-0">
						<li class="nav-item dropdown">
							<a class="nav-link dropdown-toggle" href="#" id="navbarDropdown" role="button" data-bs-toggle="dropdown" aria-expanded="false">
								New Project
							</a>
							<ul class="dropdown-menu" aria-labelledby="navbarDropdown">
							<li>
								<div class="dropdown-item">
									<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new">
										<div class="mb-3">
											<label class="form-label" for="name">project name:</label>
											<input type="text" name="name" id="name" class="form-control"/>
										</div>
										<input type="submit" class="btn btn-primary"/>
									</form>
								</div>
							</li>
							</ul>
						</li>
						<li class="nav-item dropdown">
							<a class="nav-link dropdown-toggle" href="#" id="navbarDropdown" role="button" data-bs-toggle="dropdown" aria-expanded="false">
								New Free Form Project
							</a>
							<ul class="dropdown-menu" aria-labelledby="navbarDropdown">
							<li>
								<div class="dropdown-item">
									<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new/freeform">
										<div class="mb-3">
											<label class="form-label" for="name">project name:</label>
											<input type="text" name="name" id="name" class="form-control"/>
										</div>
										<input type="submit" class="btn btn-primary"/>
									</form>
								</div>
							</li>
							</ul>
						</li>
					</ul>
				</div>
			</div>
		</nav>
		<div class="container-fluid">
			<div class="row">
				<div class="col">
					<h1>Projects</h1>
				</div>
			</div>
			<div class="row">
				<div class="col">
					<ul class="list-group">
					{{ range .Project }}
						<il class="list-group-item"><a href="/projects?pID={{.ID}}">{{.Name}}</a></il>
					{{ end }}
					</ul>
				</div>
			</div>

		</div>
	</body>
</html>
`

const projectTpl = `
<!doctype html>
<html lang="en">
	<head>
		<meta charset="utf-8">
		<meta name="viewport" content="width=device-width, initial-scale=1">
		<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/css/bootstrap.min.css" rel="stylesheet" integrity="sha384-+0n0xVW2eSR5OomGNYDnhzAbDsOXxcvSN1TPprVMTNDbiYZCxYbOOl7+AMvyTG2x" crossorigin="anonymous">
		<script src="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/js/bootstrap.bundle.min.js" integrity="sha384-gtEjrD/SeCtmISkJkNUaaKMoLD0//ElJ19smozuHV6z3Iehds+3Ulb9Bn9Plx0x4" crossorigin="anonymous"></script>
	</head>
	<body>
		<nav class="navbar navbar-expand-lg navbar-light bg-light">
			<div class="container-fluid">
				<a class="navbar-brand" href="/">Bubbles</a>
				<button class="navbar-toggler" type="button" data-bs-toggle="collapse" data-bs-target="#navbarSupportedContent" aria-controls="navbarSupportedContent" aria-expanded="false" aria-label="Toggle navigation">
					<span class="navbar-toggler-icon"></span>
				</button>

				<div class="collapse navbar-collapse" id="navbarSupportedContent">
					<ul class="navbar-nav me-auto mb-2 mb-lg-0">
						<li class="nav-item dropdown">
							<a class="nav-link dropdown-toggle" href="#" id="navbarDropdown" role="button" data-bs-toggle="dropdown" aria-expanded="false">
								New Project
							</a>
							<ul class="dropdown-menu" aria-labelledby="navbarDropdown">
							<li>
								<div class="dropdown-item">
									<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new">
										<div class="mb-3">
											<label class="form-label" for="name">project name:</label>
											<input type="text" name="name" id="name" class="form-control"/>
										</div>
										<input type="submit" class="btn btn-primary"/>
									</form>
								</div>
							</li>
							</ul>
						</li>
						<li class="nav-item dropdown">
							<a class="nav-link dropdown-toggle" href="#" id="navbarDropdown" role="button" data-bs-toggle="dropdown" aria-expanded="false">
								New Free Form Project
							</a>
							<ul class="dropdown-menu" aria-labelledby="navbarDropdown">
							<li>
								<div class="dropdown-item">
									<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new/freeform">
										<div class="mb-3">
											<label class="form-label" for="name">project name:</label>
											<input type="text" name="name" id="name" class="form-control"/>
										</div>
										<input type="submit" class="btn btn-primary"/>
									</form>
								</div>
							</li>
							</ul>
						</li>
					</ul>
				</div>
			</div>
		</nav>

		<div class="container-fluid">
			<div class="row">
				<div class="col">
					<h1>Project: {{ .Name }}</h1>
				</div>
			</div>

			<div class="row">
				<div class="col-6">
					<form method="POST" enctype="application/x-www-form-urlencoded" action="/store?pID={{ .PID }}">
						{{ if .Err }}
						<div>{{ .Err }}</div>
						{{ end }}
						<div>
							<table class="table table-striped table-hover">
								<thead>
									<th colspan=2 scope="col" class="text-center">... must happen before ...</th>
								</thead>
								<tbody>
								{{ range .Input }}
								<tr>
									<td class="text-center">{{ .Left }}</td>
									<td class="text-center">{{ .Right }}</td>
									<td class="text-center"><a href="/remove?left={{.Left}}&right={{.Right}}" style="text-decoration: none;">???????</a></td>
								</tr>
								{{ end }}
								<tr>
									<td class="text-center"><input type="text" name="newLeft"></td>
									<td class="text-center"><input type="text" name="newRight"></td>
									<td class="text-center"><input type="submit" onClick="javascript: (function(){document.forms[0].submit()})()" value="???" class="btn"/></td>
								</tr>
								</tbody>
							</table>
						</div>
					</form>
					<details>
						<summary>rename</summary>
						<form method="POST" enctype="application/x-www-form-urlencoded" action="/rename?pID={{ .PID }}">
							<label>from: <input type="text" name="from"></label>
							<label>to: <input type="text" name="to"></label>
							<input type="submit" onClick="javascript: (function(){document.forms[0].submit()})()" value="rename"/>
						</form>
					</details>
					<details>
						<summary>source</summary>
						<pre>
{{ .Src }}
						</pre>
					</details>
				</div>
				<div class="col-6 text-center">
					<h2>Notes</h2>
					<form method="POST" enctype="application/x-www-form-urlencoded" action="/details?pID={{ .PID }}" style="height: 75%">
					<textarea name="details" style="width: 100%; height: 100%; box-sizing:border-box" onkeydown="if(event.keyCode===9){var v=this.value,s=this.selectionStart,e=this.selectionEnd;this.value=v.substring(0, s)+'\t'+v.substring(e);this.selectionStart=this.selectionEnd=s+1;return false;}">{{ .Details }}</textarea>
					<input type="submit" value="save"/>
					</form>
				</div>
			</div>
			<div class="row">
				<div class="col-12">
					<svg style="width: 100%; overflow: auto;">
						<div style="display: flex; justify-content: center; align-items: center;">
						{{ .Output }}
						</div>
					</svg>
				</div>
			</div>
		</div>
	</body>
</html>
`

const freeformProjectTpl = `
<!doctype html>
<html lang="en">
	<head>
		<meta charset="utf-8">
		<meta name="viewport" content="width=device-width, initial-scale=1">
		<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/css/bootstrap.min.css" rel="stylesheet" integrity="sha384-+0n0xVW2eSR5OomGNYDnhzAbDsOXxcvSN1TPprVMTNDbiYZCxYbOOl7+AMvyTG2x" crossorigin="anonymous">
		<script src="https://cdn.jsdelivr.net/npm/bootstrap@5.0.1/dist/js/bootstrap.bundle.min.js" integrity="sha384-gtEjrD/SeCtmISkJkNUaaKMoLD0//ElJ19smozuHV6z3Iehds+3Ulb9Bn9Plx0x4" crossorigin="anonymous"></script>
	</head>
	<body>
		<nav class="navbar navbar-expand-lg navbar-light bg-light">
			<div class="container-fluid">
				<a class="navbar-brand" href="/">Bubbles</a>
				<button class="navbar-toggler" type="button" data-bs-toggle="collapse" data-bs-target="#navbarSupportedContent" aria-controls="navbarSupportedContent" aria-expanded="false" aria-label="Toggle navigation">
					<span class="navbar-toggler-icon"></span>
				</button>

				<div class="collapse navbar-collapse" id="navbarSupportedContent">
					<ul class="navbar-nav me-auto mb-2 mb-lg-0">
						<li class="nav-item dropdown">
							<a class="nav-link dropdown-toggle" href="#" id="navbarDropdown" role="button" data-bs-toggle="dropdown" aria-expanded="false">
								New Project
							</a>
							<ul class="dropdown-menu" aria-labelledby="navbarDropdown">
							<li>
								<div class="dropdown-item">
									<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new">
										<div class="mb-3">
											<label class="form-label" for="name">project name:</label>
											<input type="text" name="name" id="name" class="form-control"/>
										</div>
										<input type="submit" class="btn btn-primary"/>
									</form>
								</div>
							</li>
							</ul>
						</li>
						<li class="nav-item dropdown">
							<a class="nav-link dropdown-toggle" href="#" id="navbarDropdown" role="button" data-bs-toggle="dropdown" aria-expanded="false">
								New Free Form Project
							</a>
							<ul class="dropdown-menu" aria-labelledby="navbarDropdown">
							<li>
								<div class="dropdown-item">
									<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new/freeform">
										<div class="mb-3">
											<label class="form-label" for="name">project name:</label>
											<input type="text" name="name" id="name" class="form-control"/>
										</div>
										<input type="submit" class="btn btn-primary"/>
									</form>
								</div>
							</li>
							</ul>
						</li>
					</ul>
				</div>
			</div>
		</nav>

		<div class="container-fluid">
			<div class="row">
				<div class="col">
					<h1>Project: {{ .Name }}</h1>
				</div>
			</div>

			<div class="row">
				<div class="offset-3 col-6 text-center">
					<h2>Graph</h2>
					<form method="POST" enctype="application/x-www-form-urlencoded" action="/details?pID={{ .PID }}" style="height: 75%">
					<textarea name="details" style="min-height: 300px; width: 100%; height: 100%; box-sizing:border-box" onkeydown="if(event.keyCode===9){var v=this.value,s=this.selectionStart,e=this.selectionEnd;this.value=v.substring(0, s)+'\t'+v.substring(e);this.selectionStart=this.selectionEnd=s+1;return false;}">{{ .Details }}</textarea>
					<input type="submit" value="save"/>
					</form>
				</div>
			</div>
			<div class="row">
				<div class="col-12">
					<svg style="width: 100%; overflow: auto;">
						<div style="display: flex; justify-content: center; align-items: center;">
						{{ .Output }}
						</div>
					</svg>
				</div>
			</div>
		</div>
	</body>
</html>
`
