package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"github.com/c-bata/go-prompt"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/rqlite/sql"
	"github.com/smallfz/savage-wire/log"
	"github.com/smallfz/savage-wire/wire/client"
	"golang.org/x/term"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"runtime"
	"strings"
	"syscall"
)

func init() {
	table.StyleDefault.Format.Header = text.FormatDefault
	table.StyleDefault.Format.HeaderAlign = text.AlignCenter
}

type stmtInfo struct {
	cmd     string
	dml     bool
	rawNode any
}

func parseSQLite(q string) ([]*stmtInfo, error) {
	src := strings.NewReader(q)
	parser := sql.NewParser(src)
	sts3, err := parser.ParseStatements()
	if err != nil {
		return nil, err
	}
	results := make([]*stmtInfo, len(sts3))
	for i, t := range sts3 {
		log.Debug(fmt.Sprintf("node: %T, %v", t, t))
		cmd := ""
		switch t.(type) {
		case *sql.SelectStatement:
			cmd = "SELECT"
		case *sql.DeleteStatement:
			cmd = "DELETE"
		case *sql.UpdateStatement:
			cmd = "UPDATE"
		case *sql.InsertStatement:
			cmd = "INSERT"
		}
		info := &stmtInfo{cmd: cmd, rawNode: t}
		results[i] = info
	}
	return results, nil
}

func printRows(rows client.Rows) {
	cols := rows.Columns()
	tw := table.NewWriter()

	rowHead := make(table.Row, len(cols))
	for i, col := range cols {
		rowHead[i] = col
	}
	tw.AppendHeader(rowHead)

	for {
		row := make([]any, len(cols))
		if err := rows.NextRow(row); err != nil {
			break
		}
		trow := make(table.Row, len(row))
		for i, v := range row {
			trow[i] = v
		}
		tw.AppendRows([]table.Row{trow})
	}
	fmt.Println(tw.Render())
	// fmt.Println(strings.Join(cols, "|"))
	// for {
	//     row := make([]any, len(cols))
	//     if err := rows.NextRow(row); err != nil {
	//         break
	//     }
	//     for i, v := range row {
	//         if i > 0 {
	//             fmt.Printf("|")
	//         }
	//         fmt.Printf("%v", v)
	//     }
	//     fmt.Println("")
	// }
}

func removeCRLF(text string) string {
	pt := regexp.MustCompile(`(?is)[\r\n]+`)
	return pt.ReplaceAllString(text, " ")
	// text = strings.ReplaceAll(text, "\r", " ")
	// text = strings.ReplaceAll(text, "\n", "")
	// return text
}

var (
	candidates = []prompt.Suggest{}
)

func updateCandidates(x context.Context, conn client.Conn) {
	q := "select name from sqlite_master where type='table'"
	if stmt, err := conn.PrepareStmt(x, q); err != nil {
		fmt.Printf("prepare: %v\r\n", err)
	} else {
		defer stmt.Close()
		if rs, err := stmt.QueryWithArgs(x, nil); err != nil {
			fmt.Printf("query: %v\r\n", err)
		} else {
			defer rs.Close()
			names := []string{}
			for {
				row := make([]any, 1)
				err := rs.NextRow(row)
				if err != nil {
					break
				}
				if name, ok := row[0].(string); ok {
					names = append(names, name)
				}
			}
			cl := make([]prompt.Suggest, len(names))
			for i, name := range names {
				cl[i] = prompt.Suggest{Text: name}
			}
			cl = append(cl, prompt.Suggest{
				Text: "sqlite_master",
			})
			cl = append(cl, prompt.Suggest{
				Text: "sqlite_schema",
			})
			candidates = cl
			// fmt.Printf("candidates: %s\r\n", strings.Join(names, ","))
		}
	}
}

func getSuggestions() []prompt.Suggest {
	return candidates
}

func completer(d prompt.Document) []prompt.Suggest {
	// return nil
	// s := []prompt.Suggest{
	// 	{Text: "\\exit", Description: ""},
	// 	{Text: "?", Description: ""},
	// }
	hint := d.GetWordBeforeCursor()
	if len(hint) > 0 {
		s := getSuggestions()
		return prompt.FilterHasPrefix(s, hint, true)
	}
	return nil
}

type conf struct {
	uri string
	uid string
	pwd string
}

func main() {
	defer recoverTerm()

	cfg := &conf{
		uri: "wss://root@localhost:3099/demo",
	}
	flag.StringVar(&cfg.uri, "s", cfg.uri, "Server URI.")
	flag.Parse()

	uri, err := url.Parse(cfg.uri)
	if err != nil {
		fmt.Printf("Server URI: %v\r\n", err)
		return
	} else {
		uid, pwd := "", cfg.pwd
		if uri.User != nil {
			uid = uri.User.Username()
			if v, ok := uri.User.Password(); ok {
				pwd = v
			}
		}
		if len(uid) == 0 {
			if v, err := user.Current(); err == nil {
				uid = v.Username
			}
		}
		if len(uid) == 0 {
			uid = "root"
		}
		if len(pwd) == 0 {
			fmt.Printf("password for %s: ", uid)
			if pwdb, err := term.ReadPassword(syscall.Stdin); err == nil {
				pwd = string(pwdb)
			}
			fmt.Println("")
		}
		userInfo := url.UserPassword(uid, pwd)
		uri.User = userInfo
		cfg.uid = uid
	}

	log.SetLevel(slog.LevelWarn)

	fmt.Printf("Connecting to %s@%s...\r\n", cfg.uid, uri.Hostname())
	conn, err := client.Open(uri.String())
	if err != nil {
		fmt.Printf("Open: %v\r\n", err)
		return
	}
	defer conn.Close()
	fmt.Printf("connected as user %s.\r\n", uri.User.Username())

	fmt.Println("Welcome to Savage-DB CLI!")

	x, cancel := context.WithCancel(context.Background())
	defer cancel()

	updateCandidates(x, conn)

	buf := new(bytes.Buffer)

	runQuery := func(q string) {
		stmt, err := conn.PrepareStmt(x, q)
		if err != nil {
			fmt.Printf("prepare: %v\r\n", err)
			return
		}
		defer stmt.Close()
		rows, err := stmt.QueryWithArgs(x, nil)
		if err != nil {
			fmt.Printf("query: %v\r\n", err)
			return
		}
		defer rows.Close()
		printRows(rows)
	}

	runExec := func(cmdPrefix, q string) {
		rs, err := conn.ExecWithArgs(x, q, nil)
		if err != nil {
			fmt.Printf("exec: %v\r\n", err)
			return
		}
		if len(cmdPrefix) == 0 {
			fmt.Println("OK")
			return
		}
		if rc, err := rs.RowsAffected(); err != nil {
			fmt.Printf("%s: ok\r\n", cmdPrefix)
		} else {
			fmt.Printf("%s: %d\r\n", cmdPrefix, rc)
		}
	}

	runSQL := func(cmdPrefix, q string) {
		if strings.EqualFold(cmdPrefix, "SELECT") {
			runQuery(q)
		} else {
			runExec(cmdPrefix, q)
		}
	}

	isFragment := func() bool {
		if buf.Len() > 0 {
			q := strings.TrimSpace(string(buf.Bytes()))
			if strings.HasSuffix(q, ";") {
				if _, err := parseSQLite(q); err == nil {
					return false
				}
			}
			return true
		}
		return false
	}

	getLivePrefix := func() (prefix string, live bool) {
		return "", isFragment()
	}

	history := []string{}

	evalLine := func(line string) {
		if buf.Len() > 0 && len(line) > 0 {
			buf.Write([]byte("\r\n"))
		}
		buf.Write([]byte(line))
		q := strings.TrimSpace(string(buf.Bytes()))
		switch q {
		case "?", "help", ".help":
			buf.Reset()
			fmt.Println("type .exit to quit.")
			return
		}
		if strings.HasSuffix(q, ";") {
			history = append(history, removeCRLF(q))
			if stmts, err := parseSQLite(q); err == nil && len(stmts) > 0 {
				t := stmts[0]
				runSQL(t.cmd, q)
			}
			buf.Reset()
		}
	}

	// exitChecker := func(line string, breakLine bool) bool {
	// 	if breakLine {
	// 		switch strings.ToLower(strings.TrimSpace(line)) {
	// 		case ".exit", ".quit":
	// 			return true
	// 		}
	// 	}
	// 	return false
	// }

	// p := prompt.New(
	// 	evalLine,
	// 	completer,
	// 	prompt.OptionSetExitCheckerOnInput(exitChecker),
	// 	prompt.OptionLivePrefix(getLivePrefix),
	// )
	// p.Run()

	prefix := "> "
	for {
		content := prompt.Input(
			prefix,
			completer,
			prompt.OptionHistory(history),
			prompt.OptionLivePrefix(getLivePrefix),
			prompt.OptionPrefix(prefix),
			prompt.OptionPrefixTextColor(prompt.Blue),
			prompt.OptionSwitchKeyBindMode(prompt.EmacsKeyBind),
		)
		content = strings.TrimSpace(strings.ToLower(content))
		switch content {
		case "exit", "quit":
			return
		}
		evalLine(content)
	}
}

func recoverTerm() {
	if !strings.EqualFold(runtime.GOOS, "windows") {
		cmd := exec.Command("stty", "sane")
		cmd.Stdin = os.Stdin
		cmd.Run()
		cmd.Wait()
	}
}
