package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os/exec"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
)

type dep struct {
	Left, Right string
}

type graph struct {
	PID             string
	Name            string
	Input           []dep
	Output          template.HTML
	Err             string
	Src             string
	AllKnownBubbles []string
	Vertical        bool
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

	baseTpl := template.Must(template.New("base").Parse(baseTemplate))

	var dbMu sync.Mutex
	db, err := sql.Open("sqlite3", "state.db")
	check(err)
	defer func() {
		check(db.Close())
	}()

	sqlStmt := `
	create table if not exists pairs (project bigint, left text, right text);
	create table if not exists bubbles (project bigint, bubble text, state text);
	create unique index if not exists bubbles_project_bubble ON bubbles (project, bubble);
	create table if not exists projects (project integer primary key autoincrement, name text);
	create unique index if not exists pairs_unique on pairs (project, left, right);
	`
	_, err = db.Exec(sqlStmt)
	check(err)

	http.HandleFunc("GET /flip", func(w http.ResponseWriter, r *http.Request) {
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
		seeOtherURL := fmt.Sprintf("/projects?pID=%v", pID)
		if r.URL.Query().Has("vertical") {
			seeOtherURL += "&vertical"
		}
		http.Redirect(w, r, seeOtherURL, http.StatusSeeOther)
	})

	http.HandleFunc("DELETE /remove", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		pID := r.URL.Query().Get("pID")
		_, err := db.Exec("delete from pairs where left = ? and right = ? and project = ?", r.URL.Query().Get("left"), r.URL.Query().Get("right"), pID)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		seeOtherURL := fmt.Sprintf("/projects?pID=%v", pID)
		if r.URL.Query().Has("vertical") {
			seeOtherURL += "&vertical"
		}
		w.Header().Set("HX-Location", seeOtherURL)
	})

	http.HandleFunc("POST /rename", func(w http.ResponseWriter, r *http.Request) {
		pID := r.URL.Query().Get("pID")
		dbMu.Lock()
		defer dbMu.Unlock()
		if err := r.ParseForm(); err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest)+":"+err.Error(), http.StatusBadRequest)
			return
		}
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
		seeOtherURL := fmt.Sprintf("/projects?pID=%v", pID)
		if r.URL.Query().Has("vertical") {
			seeOtherURL += "&vertical"
		}
		w.Header().Set("HX-Location", seeOtherURL)
	})

	http.HandleFunc("POST /delete", func(w http.ResponseWriter, r *http.Request) {
		pID := r.URL.Query().Get("pID")
		dbMu.Lock()
		defer dbMu.Unlock()
		if err := r.ParseForm(); err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest)+":"+err.Error(), http.StatusBadRequest)
			return
		}
		tx, err := db.Begin()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec("delete from pairs where project = ? and (left = ? or right = ?)", pID, r.PostForm.Get("activity"), r.PostForm.Get("activity")); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		seeOtherURL := fmt.Sprintf("/projects?pID=%v", pID)
		if r.URL.Query().Has("vertical") {
			seeOtherURL += "&vertical"
		}
		w.Header().Set("HX-Location", seeOtherURL)
	})

	http.HandleFunc("POST /store", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		if err := r.ParseForm(); err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest)+":"+err.Error(), http.StatusBadRequest)
			return
		}
		tx, err := db.Begin()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		stmt, err := tx.Prepare("insert into pairs (project, left, right) values (?, ?, ?) on conflict (project, left, right) do nothing")
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		pID := r.URL.Query().Get("pID")
		newCenter := strings.TrimSpace(r.PostForm.Get("newCenter"))
		newLeft := strings.TrimSpace(r.PostForm.Get("newLeft"))
		newRight := strings.TrimSpace(r.PostForm.Get("newRight"))
		if pID != "" && newCenter != "" && newRight != "" {
			if _, err := stmt.Exec(pID, newCenter, newRight); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if pID != "" && newLeft != "" && newCenter != "" {
			if _, err := stmt.Exec(pID, newLeft, newCenter); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}

		seeOtherURL := fmt.Sprintf("/projects?pID=%v", pID)
		if r.URL.Query().Has("vertical") {
			seeOtherURL += "&vertical"
		}
		w.Header().Set("HX-Location", seeOtherURL)
	})

	http.HandleFunc("POST /projects/new", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest)+":"+err.Error(), http.StatusBadRequest)
			return
		}
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
		seeOtherURL := fmt.Sprintf("/projects?pID=%v", pID)
		w.Header().Set("HX-Location", seeOtherURL)
	})

	http.HandleFunc("DELETE /projects", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		pID := r.URL.Query().Get("pID")
		if _, err := db.Exec("DELETE FROM pairs WHERE project = ?", pID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := db.Exec("DELETE FROM bubbles WHERE project = ?", pID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := db.Exec("DELETE FROM projects WHERE project = ?", pID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	})

	renderProjectTpl := template.Must(template.Must(baseTpl.Clone()).New("content").Parse(renderProjectTemplate))
	http.HandleFunc("GET /projects", func(w http.ResponseWriter, r *http.Request) {
		pID := r.URL.Query().Get("pID")
		if err := r.ParseForm(); err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest)+":"+err.Error(), http.StatusBadRequest)
			return
		}
		var deps []dep
		dbMu.Lock()
		defer dbMu.Unlock()

		rowsPairs, err := db.Query("select left, right from pairs where project = ?", pID)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() {
			if err := rowsPairs.Close(); err != nil {
				log.Printf("cannot close rowsPairs: %v", err)
			}
		}()
		input := &bytes.Buffer{}
		knownBubblesIdx := make(map[string]struct{})
		allKnownBubbles := make(map[string]struct{})
		fmt.Fprintln(input, "digraph G {")
		if !r.URL.Query().Has("vertical") {
			fmt.Fprintln(input, `	rankdir="LR"`)
		}
		for rowsPairs.Next() {
			var dep dep
			if err := rowsPairs.Scan(&dep.Left, &dep.Right); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
				return
			}
			knownBubblesIdx[dep.Left] = struct{}{}
			knownBubblesIdx[dep.Right] = struct{}{}
			allKnownBubbles[dep.Left] = struct{}{}
			allKnownBubbles[dep.Right] = struct{}{}
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
		defer func() {
			if err := rowsBubbles.Close(); err != nil {
				log.Printf("cannot close rowsBubbles: %v", err)
			}
		}()
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

		download := r.URL.Query().Has("download")
		if download {
			cmd := exec.CommandContext(r.Context(), "dot", "-Tpng")
			cmd.Stdin = input
			var outBuf bytes.Buffer
			cmd.Stdout = &outBuf
			if err := cmd.Run(); err != nil {
				log.Println(err)
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Disposition", `attachment; filename="graph.png"`)
			if _, err := io.Copy(w, &outBuf); err != nil {
				log.Println(err)
			}
			return
		}

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
		allKnownBubblesList := maps.Keys(allKnownBubbles)
		slices.Sort(allKnownBubblesList)
		var projectName string
		rowProject := db.QueryRow("select name from projects where project = ?", pID)
		if err := rowProject.Scan(&projectName); err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		err = renderProjectTpl.ExecuteTemplate(w, "base", graph{
			PID:             pID,
			Name:            projectName,
			Input:           deps,
			Output:          template.HTML(outBuf.String()),
			Err:             errBuf.String(),
			Src:             src,
			AllKnownBubbles: allKnownBubblesList,
			Vertical:        r.URL.Query().Has("vertical"),
		})
		if err != nil {
			log.Printf("cannot execute template: %v", err)
		}
	})

	listProjectsTpl := template.Must(template.Must(baseTpl.Clone()).New("content").Parse(listProjectsTemplate))
	http.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		dbMu.Lock()
		defer dbMu.Unlock()
		rows, err := db.Query("select project, name from projects", r.URL.Query().Get("left"), r.URL.Query().Get("right"))
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError)+":"+err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() {
			if err := rows.Close(); err != nil {
				log.Printf("cannot close rows: %v", err)
			}
		}()
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
		err = listProjectsTpl.ExecuteTemplate(w, "base", struct {
			Project []project
		}{projects})
		if err != nil {
			log.Printf("cannot execute template: %v", err)
		}
	})
	const bindAddr = "0.0.0.0:5466"
	log.Println("Starting server on http://" + bindAddr)
	check(http.ListenAndServe(bindAddr, nil))
}

func check(err error) {
	if err != nil {
		debug.PrintStack()
		log.Fatal(err)
	}
}

const baseTemplate = `
{{ define "base" }}
<!doctype html>
<html lang="en" data-theme="light">
	<head>
		<meta charset="utf-8">
		<meta name="viewport" content="width=device-width, initial-scale=1">
		<script src="https://cdn.jsdelivr.net/npm/htmx.org@2.0.6/dist/htmx.min.js"></script>
		<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@picocss/pico@2/css/pico.min.css">
		<style>
			#svg-container { text-align: center; }
			#svg-container svg { max-width: 100%; height: auto; }
			#svg-container svg a { text-decoration: none; color: black; width: 100%;  }
		</style>
	</head>
	<body hx-boost="true">
		<header class="container">
			<nav>
				<ul>
					<li>
						<a href="/" class="secondary">
							<strong>Bubbles</strong>
						</a>
					</li>
				</ul>
				<ul>
					<li>
						<details class="dropdown">
							<summary>new project</summary>
							<ul>
								<li>
									<form method="POST" enctype="application/x-www-form-urlencoded" action="/projects/new">
										<div>
											<label for="name">project name</label>
											<input type="text" name="name" id="name"/>
										</div>
										<input type="submit" value="create"/>
									</form>
								</li>
							</ul>
						</details>
					</li>
				</ul>
			</nav>
		</header>
		<main class="container">
		{{ template "content" . }}
		</main>
	</body>
</html>
{{ end }}
`

const listProjectsTemplate = `
<strong>Projects</strong>
{{ with .Project }}
{{ range . }}
<ul>
	<li>
		<a href="/projects?pID={{.ID}}">{{.Name}}</a>
		<a hx-delete="/projects?pID={{.ID}}" style="text-decoration: none;" hx-confirm="Are you sure you want to delete this project?">🗑️</a>
	</li>
</ul>
{{ end }}
{{ else }}
<p>no projects yet</p>
{{ end }}
`

const renderProjectTemplate = `
{{- $pid := .PID -}}
<strong>Project: {{ .Name }}</strong>
<section>
<div class="grid">
	<div>
		<a href="/projects?pID={{ .PID }}&download{{ if .Vertical }}&vertical{{end}}" class="secondary">download</a>
		<a href="javascript: copyImageToClipboard()" class="secondary">copy</a>
		{{ if .Vertical }}
		<a href="/projects?pID={{ .PID }}" class="secondary">horizontal</a>
		{{ else }}
		<a href="/projects?pID={{ .PID }}&vertical" class="secondary">vertical</a>
		{{ end }}
	</div>
</div>
</section>
<section>
	<div class="grid">
		<div id="svg-container">
			{{ .Output }}
		</div>
	</div>
</section>
<section>
<div class="grid">
	<div>
		<article>
			<details>
				<summary>rename</summary>
				<form method="POST" enctype="application/x-www-form-urlencoded" action="/rename?pID={{ .PID }}{{ if .Vertical }}&vertical{{ end }}">
					<label>from: <input type="text" list="knownBubbles" name="from"></label>
					<label>to: <input type="text" name="to"></label>
					<input type="submit" value="rename"/>
				</form>
			</details>
		</article>
	</div>
	<div>
		<article>
			<details>
				<summary>delete</summary>
				<form method="POST" enctype="application/x-www-form-urlencoded" action="/delete?pID={{ .PID }}{{ if .Vertical }}&vertical{{ end }}">
					<label>activity: <input type="text" list="knownBubbles" name="activity"></label>
					<input type="submit" value="delete"/>
				</form>
			</details>
		</article>
	</div>
	<div>
		<article>
			<details>
				<summary>source</summary>
				<pre>
{{ .Src }}
				</pre>
			</details>
		</article>
	</div>
</div>
<div class="grid">
	<div>
		<hr/>
	</div>
</div>
<div class="grid">
	<div>
		<datalist id="knownBubbles">
		{{ range .AllKnownBubbles }}
			<option>{{- . -}}</option>
		{{ end }}
		</datalist>
		{{ if .Err }}
			<div>{{ .Err }}</div>
		{{ end }}
		<form method="POST" enctype="application/x-www-form-urlencoded" action="/store?pID={{ .PID }}{{ if .Vertical }}&vertical{{ end }}">
			<fieldset class="grid">
				<input type="text" list="knownBubbles" id="newLeft" name="newLeft" onKeyUp="javascript: filter()">
				<input type="text" list="knownBubbles" id="newCenter" name="newCenter" onKeyUp="javascript: filter()">
				<input type="text" list="knownBubbles" id="newRight" name="newRight" onKeyUp="javascript: filter()">
				<input type="submit" value="➕" class="outline contrast"/>
			</fieldset>
		</form>
	</div>
</div>
<div class="grid">
	<div>
		<table>
			<tbody id="pairsTableBody" class="striped">
			{{ $vertical := .Vertical }}
			{{ range .Input }}
			<tr id="pair-{{ .Left }}-{{ .Right }}-{{ $pid }}" data-left="{{ .Left }}" data-right="{{ .Right }}">
				<td>{{ .Left }}</td>
				<td>{{ .Right }}</td>
				<td><button hx-delete="/remove?pID={{ $pid }}&left={{.Left}}&right={{.Right}}{{ if $vertical }}&vertical{{ end }}" class="outline contrast">🗑️</button></td>
			</tr>
			{{ end }}
			</tbody>
		</table>
	</div>
</div>
</section>
<script>
function setCookie(value) {
	document.cookie = "bubbles=" + encodeURIComponent(JSON.stringify(value));
}
function getCookie() {
	var ca = document.cookie.split(';');
	for (const kv of ca) {
		if (kv.trim().startsWith("bubbles=")){
			try {
				return JSON.parse(decodeURIComponent(kv.trim().split("=")[1]))
			} catch (e) {
			}
			break
		}
	}
	return {'left':'','center':'','right':''}
}
window.onload = function() {
	const v = getCookie()
	document.getElementById("newLeft").value =  v.left
	document.getElementById("newCenter").value =  v.center
	document.getElementById("newRight").value = v.right
	filter()
};

function filter() {
	let center = document.getElementById("newCenter").value.toLowerCase()
	let left = document.getElementById("newLeft").value.toLowerCase()
	let right = document.getElementById("newRight").value.toLowerCase()
	setCookie({
		'left':document.getElementById("newLeft").value,
		'center':document.getElementById("newCenter").value,
		'right':document.getElementById("newRight").value
	})

	let pairsTable = document.getElementById("pairsTableBody")
	for (const tr of pairsTable.children) {
		tr.style = 'display: table-row'
	}
	if (center == "" && left == "" && right == "") {
		return
	}
	for (const tr of pairsTable.children) {
		tr.style = 'display: none'
		if (center == "" && left != "" && right != "" && (tr.dataset.left.toLowerCase().includes(left) || tr.dataset.right.toLowerCase().includes(right))) {
			tr.style = 'display: table-row'
		} else if (center != "" && (tr.dataset.left.toLowerCase().includes(center) || tr.dataset.right.toLowerCase().includes(center))) {
			tr.style = 'display: table-row'
		} else if (left != "" && tr.dataset.left.toLowerCase().includes(left)) {
			tr.style = 'display: table-row'
		} else if (right != "" && tr.dataset.right.toLowerCase().includes(right)) {
			tr.style = 'display: table-row'
		}
	}
}
async function copyImageToClipboard() {
	try {
		const response = await fetch("/projects?pID={{ .PID }}&download{{ if .Vertical }}&vertical{{end}}");
		const blob = await response.blob();
		await navigator.clipboard.write([
			new ClipboardItem({[blob.type]: blob})
		]);
	} catch (e) {
		console.error("cannot copy image to clipboard", e);
	}
}
</script>
`
