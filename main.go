package main

import (
	"bufio"
	"bytes"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	lgtable "charm.land/lipgloss/v2/table"
	"context"
	"encoding/json"
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
					if len(lineb) <= 0 {
						continue
					}
					line := ""
					if err := json.Unmarshal(lineb, &line); err != nil {
						line = string(lineb)
					}
					lines = append(lines, line)
				}
			}
		}
	}
	return lines
}

var (
	chHistNewLine = make(chan string, 32)
)

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
	h.lines = append(h.lines, line)
	if dat, err := json.Marshal(line); err == nil {
		select {
		case chHistNewLine <- string(dat):
		case <-time.After(time.Second):
		}
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

func (h *hist) last10() []string {
	start := len(h.lines) - 10
	if start < 0 {
		start = 0
	}
	lasts := h.lines[start:]
	out := make([]string, len(lasts))
	for i, line := range lasts {
		out[len(out)-i-1] = line
	}
	return out
}

func isIOEndErr(err error) bool {
	if err != nil {
		switch err {
		case io.EOF, io.ErrClosedPipe:
			return true
		}
	}
	return false
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
		// fmt.Fprintf(os.Stderr, "parseStatements: %v\r\n", err)
		return nil, err
	}
	results := make([]*stmtInfo, len(sts3))
	for i, t := range sts3 {
		// fmt.Fprintf(os.Stderr, "node: %T, %v\r\n", t, t)
		cmd := ""
		switch t.(type) {
		case *sql.AnalyzeStatement:
			cmd = "ANALYZE"
		case *sql.ExplainStatement:
			cmd = "EXPLAIN"
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

func printRowsLegacy(w io.Writer, rows client.Rows) {
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

func printRows(w io.Writer, rows client.Rows) int {
	cols := rows.Columns()
	tbl := lgtable.New().Headers(cols...)

	count := 0

	for {
		row := make([]any, len(cols))
		if err := rows.NextRow(row); err != nil {
			break
		}
		texts := make([]string, len(row))
		for i, v := range row {
			if s, ok := v.(fmt.Stringer); ok {
				texts[i] = s.String()
			} else {
				texts[i] = fmt.Sprintf("%v", v)
			}
		}
		tbl.Row(texts...)
		count += 1
	}
	fmt.Fprintln(w, tbl.Render())

	return count
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

		x1 := context.WithValue(x, "conn", conn)

		if fatal, err := replv4(x1, conn); err != nil {
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

	t0 := time.Now()

	stmt, err := conn.PrepareStmt(x, q)
	if err != nil {
		fmt.Fprintf(w, "prepare: %T,%v\r\n", err, err)
		return err
	}
	defer stmt.Close()

	rows, err := stmt.QueryWithArgs(x, nil)
	if err != nil {
		fmt.Fprintf(w, "query: %v\r\n", err)
		return err
	}
	defer rows.Close()

	rowsCount := printRows(w, rows)

	dur := time.Now().Sub(t0)
	remark := grayText.Render(fmt.Sprintf(
		"(%d rows in %s)", rowsCount, dur,
	))
	fmt.Fprintln(w, remark)

	return nil
}

func runExec(x context.Context, w io.Writer, cmdPrefix, q string) error {
	conn := x.Value("conn").(client.Conn)

	rs, err := conn.ExecWithArgs(x, q, nil)
	if err != nil {
		fmt.Fprintf(w, "exec: %v\r\n", err)
		return err
	}

	if len(cmdPrefix) == 0 {
		fmt.Fprintln(w, "OK")
		return nil
	}

	if rc, err := rs.RowsAffected(); err != nil {
		fmt.Fprintf(w, "%s: ok\r\n", cmdPrefix)
		return err
	} else {
		fmt.Fprintf(w, "%s: %d\r\n", cmdPrefix, rc)
	}

	return nil
}

func runSQL(x context.Context, w io.Writer, cmdPrefix, q string) error {
	switch strings.ToUpper(cmdPrefix) {
	case "SELECT", "EXPLAIN":
		return runQuery(x, w, q)
	default:
		return runExec(x, w, cmdPrefix, q)
	}
}

func replv1(x context.Context, conn client.Conn) (fatal bool, err error) {
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
			if isIOEndErr(err) {
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
					if isIOEndErr(err) {
						return false, err
					}
				}
			} else {
				if err := runSQL(x, t, "", q); err != nil {
					if isIOEndErr(err) {
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

type replResult struct {
	fatal bool
	err   error
}

var (
	borderStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62"))
	navyText = lipgloss.NewStyle().Foreground(lipgloss.Color("103"))
	grayText = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

type tuiModel struct {
	hist        *hist
	histCur     int
	vp          viewport.Model
	scrollTop   int
	text        textarea.Model
	heightTotal int
	ch          chan string
	replResult  *replResult
	chClose     chan byte
}

var _ tea.Model = (*tuiModel)(nil)

func newTUIModel() *tuiModel {
	ta := textarea.New()
	ta.SetHeight(5)
	ta.Placeholder = "Alt+Enter to run SQL..."
	ta.Focus()

	vp := viewport.New(viewport.WithWidth(30), viewport.WithHeight(5))
	welcome := " Welcome to Savage-DB CLI!\n " + grayText.Render("Ctrl+Q to exit.")
	vp.SetContent(welcome)
	vp.MouseWheelEnabled = true
	vp.SoftWrap = true

	return &tuiModel{
		hist:    newSimpleHistory(),
		vp:      vp,
		text:    ta,
		ch:      make(chan string),
		chClose: make(chan byte, 1),
	}
}

func (t *tuiModel) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
	)
}

func (t *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	routeToVp := false
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		t.heightTotal = m.Height
		t.text.SetWidth(m.Width - 2)
		t.vp.SetWidth(m.Width - 2)
		t.vp.SetHeight(m.Height - t.text.Height() - 2)
		t.vp.GotoBottom()
		return t, nil
	case tea.MouseWheelMsg:
		routeToVp = true
	case tea.KeyMsg:
		switch m.String() {
		case "ctrl+q":
			select {
			case t.chClose <- 1:
			default:
			}
			return t, tea.Quit
		case "meta+enter", "alt+enter":
			content := t.text.Value()
			select {
			case t.ch <- content:
			default:
				return t, nil
			}
			t.histCur = 0
			return t, nil
		case "up":
			t.histCur += 1
			// t.vp.SetContent(fmt.Sprintf("histCur: %d", t.histCur))
			t.text.SetValue(t.hist.At(t.histCur - 1))
			return t, nil
		case "down":
			t.histCur -= 1
			if t.histCur < 0 {
				t.histCur = 0
			}
			// t.vp.SetContent(fmt.Sprintf("histCur: %d", t.histCur))
			t.text.SetValue(t.hist.At(t.histCur - 1))
			return t, nil
		default:
			t.histCur = 0
		}
	case tea.KeyPressMsg:
		switch m.String() {
		case "ctrl+r":
			t.histCur = 0
			content := t.text.Value()
			t.vp.SetContent(content)
			t.text.Reset()
			t.vp.GotoBottom()
			return t, nil
		}
	}

	cmds := []tea.Cmd{}
	if routeToVp {
		if vp, cmd := t.vp.Update(msg); true {
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			t.vp = vp
		}
	} else {
		if txt, cmd := t.text.Update(msg); true {
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			t.text = txt
		}
	}

	return t, tea.Batch(cmds...)
}

func (t *tuiModel) View() tea.View {
	// return tea.NewView()
	vpView := borderStyle.Render(t.vp.View())
	v := tea.NewView(vpView + "\n" + t.text.View())
	v.MouseMode = tea.MouseModeAllMotion
	return v
}

func (t *tuiModel) handleSQL(x context.Context) {
	for {
		select {
		case <-x.Done():
			return
		case <-t.chClose:
			t.replResult = &replResult{fatal: true}
			return
		case q := <-t.ch:
			switch q {
			case "exit", "quit":
				t.replResult = &replResult{fatal: true}
				return
			}
			t.hist.add(q)
			if fatal, err := t.run(x, q); fatal {
				t.replResult = &replResult{fatal: fatal, err: err}
				return
			}
		}
	}
}

func (t *tuiModel) run(x0 context.Context, q string) (fat bool, err error) {
	buf := new(bytes.Buffer)

	defer func() {
		t.text.Reset()
		content := string(buf.Bytes())
		t.vp.SetContent(content)
		// h := lipgloss.Height(content)
		// if h > t.heightTotal-t.text.Height() {
		// 	// t.vp.SetHeight(h)
		// 	t.vp.SetHeight(t.heightTotal - t.text.Height())
		// } else {
		// 	t.vp.SetHeight(t.heightTotal - t.text.Height())
		// }
		t.vp.GotoTop()
	}()

	x, cancel := context.WithCancel(x0)
	defer cancel()

	go func() {
		/* loading spinner */
		i := 0
		runes := []rune("|/-\\")
		size := len(runes)
		for {
			select {
			case <-x.Done():
				return
			case <-time.After(time.Millisecond * 150):
				offset := i % size
				content := string(runes[offset : offset+1])
				t.vp.SetContent(grayText.Render(content))
				i += 1
			}
		}
	}()

	if stmts, err := parseSQLite(q); err == nil && len(stmts) > 0 {
		st := stmts[0]
		if err := runSQL(x, buf, st.cmd, q); err != nil {
			if isIOEndErr(err) {
				return false, err
			}
		}
	} else {
		if err := runSQL(x, buf, "", q); err != nil {
			if isIOEndErr(err) {
				return false, err
			}
		}
	}
	return false, nil
}

func replv2(x context.Context, conn client.Conn) (fatal bool, err error) {
	x1, cancel := context.WithCancel(x)
	defer cancel()

	mod := newTUIModel()
	p := tea.NewProgram(mod)

	go func() {
		mod.handleSQL(x1)
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		return true, err
	}

	if mod.replResult == nil {
		return false, nil
	}

	return mod.replResult.fatal, mod.replResult.err
}

func replv3(x context.Context, conn client.Conn) (fatal bool, err error) {
	defer fmt.Println("")

	h := newSimpleHistory()
	t := os.Stdout

	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+q"))
	km.Input.AcceptSuggestion = key.NewBinding(key.WithKeys("tab"))
	km.Text.NewLine = key.NewBinding(key.WithKeys(
		"shift-enter", "ctrl+j", "alt+enter",
	))
	km.Text.Submit = key.NewBinding(key.WithKeys("enter"))

	// lines := []string{}
	for {
		line := ""

		// input := huh.NewInput().Value(&line)
		// if len(lines) == 0 {
		// 	input = input.Prompt("> ")
		// } else {
		// 	input = input.Prompt("> ")
		// }
		// input.SuggestionsFunc(func() []string {
		// 	return h.last10()
		// }, nil)

		input := huh.NewText().
			ExternalEditor(false).
			Placeholder("").
			Lines(4).
			Value(&line)

		form := huh.NewForm(huh.NewGroup(input)).WithKeyMap(km)
		if err := form.Run(); err != nil {
			return true, nil
		}

		line = strings.TrimSpace(removeCRLF(line))

		switch line {
		case "exit", "quit":
			return true, nil
		}

		if len(line) > 0 {
			// lines = append(lines, line)
			fmt.Fprintf(t, ">> %s\r\n", line)
		}

		if true || strings.HasSuffix(line, ";") {
			// q := strings.Join(lines, " ")
			q := line
			// lines = lines[:0]
			h.add(q)
			/* run the SQL. */
			if stmts, err := parseSQLite(q); err == nil && len(stmts) > 0 {
				st := stmts[0]
				if err := runSQL(x, t, st.cmd, q); err != nil {
					if isIOEndErr(err) {
						return false, err
					}
				}
			} else {
				if err := runSQL(x, t, "", q); err != nil {
					if isIOEndErr(err) {
						return false, err
					}
				}
			}
		}
	}
	return true, nil
}

func replv4(x context.Context, conn client.Conn) (fatal bool, err error) {
	t := os.Stdout
	fmt.Fprintln(t, navyText.Render("ctrl+q to exit."))

	h := newSimpleHistory()

	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+q"))
	km.Input.AcceptSuggestion = key.NewBinding(key.WithKeys("tab"))
	km.Text.NewLine = key.NewBinding(key.WithKeys(
		"shift-enter", "ctrl+j", "alt+enter",
	))
	km.Text.Submit = key.NewBinding(key.WithKeys("enter"))

	for {
		ta := &textAccessor{}

		text := huh.NewText().
			ExternalEditor(false).
			Lines(4).
			Accessor(ta)

		form := huh.NewForm(huh.NewGroup(text)).
			WithKeyMap(km).
			WithShowErrors(true)

		mod := newTUIModelV4(h, form, text, ta)
		p := tea.NewProgram(mod)

		if model, err := p.Run(); err != nil {
			return true, err
		} else if t, ok := model.(*tuiModelV4); ok {
			if t.quit {
				return true, nil
			}
		}

		line := ta.Get()
		line = strings.TrimSpace(line)

		if len(line) == 0 {
			continue
		} else {
			fmt.Fprintf(t, ">> %s\r\n", line)
		}

		switch line {
		case "exit", "quit":
			return true, nil
		}

		q := line
		h.add(q)
		/* run the SQL. */
		if stmts, err := parseSQLite(q); err == nil && len(stmts) > 0 {
			st := stmts[0]
			if err := runSQL(x, t, st.cmd, q); err != nil {
				if isIOEndErr(err) {
					return false, err
				}
			}
		} else {
			if err := runSQL(x, t, "", q); err != nil {
				if isIOEndErr(err) {
					return false, err
				}
			}
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
