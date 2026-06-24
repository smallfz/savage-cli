package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/rqlite/sql"
	"github.com/smallfz/savage-wire/log"
	"github.com/smallfz/savage-wire/wire/client"
	"golang.org/x/term"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var (
	autoReconnect = true
	historyFile   = ".savage-history"
)

func init() {
	table.StyleDefault.Format.Header = text.FormatDefault
	table.StyleDefault.Format.HeaderAlign = text.AlignCenter
}

func readPersistedHistory() []string {
	lines := []string{}
	if loc, err := os.UserHomeDir(); err == nil {
		fpath := filepath.Join(loc, historyFile)
		if f, err := os.Open(fpath); err == nil {
			defer f.Close()
			delim := byte('\n')
			r := bufio.NewReader(f)
			for {
				if lineb, err := r.ReadBytes(delim); err != nil {
					break
				} else {
					lineb = bytes.TrimSpace(lineb)
					if len(lineb) > 0 {
						lines = append(lines, string(lineb))
					}
				}
			}
		}
	}
	return lines
}

var (
	chHistNewLine = make(chan string, 32)
)

func persistHistoryLine(line string) {
}

func startHistoryAppender(x context.Context) {
	pending := make([]string, 0, 32)

	flush := func() {
		if loc, err := os.UserHomeDir(); err == nil {
			fpath := filepath.Join(loc, historyFile)
			flg := os.O_APPEND | os.O_CREATE | os.O_WRONLY
			if f, err := os.OpenFile(fpath, flg, 0600); err == nil {
				defer f.Close()
				for _, line := range pending {
					fmt.Fprintf(f, "%s\n", line)
				}
			}
		}
		pending = pending[:0]
	}

	defer flush()

	for {
		select {
		case <-x.Done():
			return
		case line := <-chHistNewLine:
			pending = append(pending, line)
			if len(pending) >= cap(pending) {
				flush()
			}
		case <-time.After(time.Second):
			if len(pending) > 0 {
				flush()
			}
		}
	}
}

type hist struct {
	lines []string
}

var _ term.History = (*hist)(nil)

func newSimpleHistory() *hist {
	h := &hist{
		lines: readPersistedHistory(),
	}
	return h
}

func (h *hist) Add(line string) {
}

func (h *hist) add(line string) {
	h.lines = append(h.lines, removeCRLF(line))
	select {
	case chHistNewLine <- line:
	case <-time.After(time.Second):
	}
}

func (h *hist) Len() int {
	return len(h.lines)
}

func (h *hist) At(i int) string {
	if i >= 0 && i < len(h.lines) {
		return h.lines[len(h.lines)-1-i]
	}
	return ""
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

func printRows(w io.Writer, rows client.Rows) {
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
	fmt.Fprintln(w, tw.Render())
}

func removeCRLF(text string) string {
	pt := regexp.MustCompile(`(?is)[\r\n]+`)
	return pt.ReplaceAllString(text, " ")
}

type conf struct {
	uri string
	uid string
	pwd string
}

func parseConf() (*conf, error) {
	cfg := &conf{
		uri: "wss://root@localhost:3099/demo",
	}
	flag.StringVar(&cfg.uri, "s", cfg.uri, "Server URI.")
	flag.Parse()

	uri, err := url.Parse(cfg.uri)
	if err != nil {
		fmt.Printf("Server URI: %v\r\n", err)
		return nil, err
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

	cfg.uri = uri.String()

	return cfg, nil
}

func makeConn(x context.Context, cfg *conf) (client.Conn, error) {
	uri, err := url.Parse(cfg.uri)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Connecting to %s@%s...\r\n", cfg.uid, uri.Hostname())
	conn, err := client.Open(uri.String())
	if err != nil {
		fmt.Printf("Open: %v\r\n", err)
		return nil, err
	}

	return conn, nil
}

func main() {
	// defer recoverTerm()

	log.SetLevel(slog.LevelWarn)

	cfg, err := parseConf()
	if err != nil {
		fmt.Printf("%v\r\n", err)
		return
	}

	x, cancel := context.WithCancel(context.Background())
	defer cancel()

	go startHistoryAppender(x)

	for {
		conn, err := makeConn(x, cfg)
		if err != nil {
			return
		}
		defer conn.Close()

		fmt.Printf("connected as user %s.\r\n", conn.AuthInfo().User)

		x = context.WithValue(x, "conn", conn)

		if fatal, err := runREPL(x, conn); err != nil {
			if fatal || !autoReconnect {
				break
			} else {
				fmt.Println("try reconnecting...")
			}
		} else {
			if fatal {
				break
			}
		}
	}
}

func runQuery(x context.Context, w io.Writer, q string) error {
	conn := x.Value("conn").(client.Conn)
	stmt, err := conn.PrepareStmt(x, q)
	if err != nil {
		fmt.Printf("prepare: %v\r\n", err)
		return err
	}
	defer stmt.Close()
	rows, err := stmt.QueryWithArgs(x, nil)
	if err != nil {
		fmt.Printf("query: %v\r\n", err)
		return err
	}
	defer rows.Close()
	printRows(w, rows)
	return nil
}

func runExec(x context.Context, w io.Writer, cmdPrefix, q string) error {
	conn := x.Value("conn").(client.Conn)
	rs, err := conn.ExecWithArgs(x, q, nil)
	if err != nil {
		fmt.Printf("exec: %v\r\n", err)
		return err
	}
	if len(cmdPrefix) == 0 {
		fmt.Println("OK")
		return nil
	}
	if rc, err := rs.RowsAffected(); err != nil {
		fmt.Printf("%s: ok\r\n", cmdPrefix)
		return err
	} else {
		fmt.Printf("%s: %d\r\n", cmdPrefix, rc)
	}
	return nil
}

func runSQL(x context.Context, w io.Writer, cmdPrefix, q string) error {
	if strings.EqualFold(cmdPrefix, "SELECT") {
		return runQuery(x, w, q)
	} else {
		return runExec(x, w, cmdPrefix, q)
	}
}

func runREPL(x context.Context, conn client.Conn) (fatal bool, err error) {
	defer fmt.Println("")

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Printf("%v\r\n", err)
		return true, err
	}
	defer term.Restore(fd, oldState)

	rw := struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stdout}

	prompt := ">> "
	t := term.NewTerminal(rw, prompt)
	h := newSimpleHistory()
	t.History = h

	fmt.Fprintln(t, "Welcome to Savage-DB CLI!")

	buffer := []string{}

	for {
		line, err := t.ReadLine()
		if err != nil {
			if err == io.EOF {
				// break // Ctrl+D, Ctrl+C
			}
			fmt.Fprintf(t, "ReadLine: %v\r\n", err)
			// break
			return true, nil
		}

		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" && len(buffer) == 0 {
			continue
		}

		buffer = append(buffer, line)

		// 检查语句是否以分号结尾
		if strings.HasSuffix(trimmedLine, ";") {
			q := strings.Join(buffer, " ")
			h.add(q)

			if stmts, err := parseSQLite(q); err == nil && len(stmts) > 0 {
				st := stmts[0]
				if err := runSQL(x, t, st.cmd, q); err != nil {
					if err == io.EOF {
						return false, err
					}
				}
			} else {
				if err := runSQL(x, t, "", q); err != nil {
					if err == io.EOF {
						return false, err
					}
				}
			}

			buffer = nil
			t.SetPrompt(prompt)
		} else {
			switch trimmedLine {
			case "exit", "quit":
				return true, nil
			}
			// 没有遇到分号，说明是多行输入，更改提示符
			t.SetPrompt("  ")
		}
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
