package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type session struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

type processInfo struct {
	PID  int
	Stat string
	CPU  float64
	TTY  string
}

type entry struct {
	PID     int
	State   string
	Name       string
	Cwd        string
	App        string
	Uptime     string
	LastActive string
	Elapsed    time.Duration
}

// messages
type refreshMsg []entry
type tickMsg struct{}
type killResultMsg struct {
	pid int
	err error
}

// styles
var (
	headerStyle   = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
	titleStyle    = lipgloss.NewStyle().Bold(true)
)

type model struct {
	entries  []entry
	cursor   int
	width    int
	message  string
	quitting bool
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func (m model) Init() tea.Cmd {
	return tea.Batch(refresh, tick())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.message = ""
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
			}
		case "k", "delete":
			if len(m.entries) > 0 {
				e := m.entries[m.cursor]
				if e.PID == os.Getpid() {
					m.message = "can't kill current session"
					return m, nil
				}
				m.entries = append(m.entries[:m.cursor], m.entries[m.cursor+1:]...)
			if m.cursor >= len(m.entries) {
				m.cursor = max(0, len(m.entries)-1)
			}
			return m, killProcess(e.PID)
			}
		}

	case tickMsg:
		return m, tea.Batch(refresh, tick())

	case refreshMsg:
		m.entries = []entry(msg)
		if m.cursor >= len(m.entries) {
			m.cursor = max(0, len(m.entries)-1)
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case killResultMsg:
		if msg.err != nil {
			m.message = fmt.Sprintf("failed to kill %d: %v", msg.pid, msg.err)
		} else {
			m.message = fmt.Sprintf("killed %d", msg.pid)
		}
		return m, refresh
	}

	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("cctop"))
	b.WriteString("\n\n")

	if len(m.entries) == 0 {
		b.WriteString("No running Claude sessions found.\n")
		b.WriteString(dimStyle.Render("\nq quit"))
		return b.String()
	}

	// Column widths
	cols := [7]string{"PID", "STATE", "NAME", "CWD", "APP", "ACTIVE", "UPTIME"}
	widths := [7]int{}
	for i, c := range cols {
		widths[i] = len(c)
	}
	for _, e := range m.entries {
		vals := rowValues(e)
		for i, v := range vals {
			if len(v) > widths[i] {
				widths[i] = len(v)
			}
		}
	}

	fmtStr := fmt.Sprintf("%%-%ds  %%-%ds  %%-%ds  %%-%ds  %%-%ds  %%-%ds  %%-%ds",
		widths[0], widths[1], widths[2], widths[3], widths[4], widths[5], widths[6])

	header := fmt.Sprintf(fmtStr, cols[0], cols[1], cols[2], cols[3], cols[4], cols[5], cols[6])
	b.WriteString(headerStyle.Render(padRight(header, m.width)))
	b.WriteString("\n")

	for i, e := range m.entries {
		vals := rowValues(e)
		line := fmt.Sprintf(fmtStr, vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], vals[6])

		if i == m.cursor {
			b.WriteString(selectedStyle.Render(padRight(line, m.width)))
		} else {
			b.WriteString(line)
		}
		if e.PID == os.Getpid() {
			b.WriteString(dimStyle.Render(" *"))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if m.message != "" {
		b.WriteString(m.message + "\n")
	}
	b.WriteString(dimStyle.Render("↑/↓: navigate   k: kill   q: quit"))
	b.WriteString("\n")

	return b.String()
}


func rowValues(e entry) [7]string {
	name := e.Name
	if name == "" {
		name = "-"
	}
	cwd := e.Cwd
	if cwd == "" {
		cwd = "-"
	}
	app := e.App
	if app == "" {
		app = "-"
	}
	active := e.LastActive
	if active == "" {
		active = "-"
	}
	uptime := e.Uptime
	if uptime == "" {
		uptime = "-"
	}
	return [7]string{
		strconv.Itoa(e.PID),
		e.State,
		name,
		cwd,
		app,
		active,
		uptime,
	}
}

// commands

func refresh() tea.Msg {
	return refreshMsg(loadEntries())
}

func killProcess(pid int) tea.Cmd {
	return func() tea.Msg {
		err := syscall.Kill(pid, syscall.SIGTERM)
		if err == nil {
			// Wait for process to actually exit
			for range 20 {
				time.Sleep(100 * time.Millisecond)
				if syscall.Kill(pid, 0) != nil {
					break
				}
			}
		}
		return killResultMsg{pid: pid, err: err}
	}
}

// data loading

func loadEntries() []entry {
	sessions := loadSessions()
	procs := getClaudeProcesses()

	var entries []entry
	matched := make(map[int]bool)

	for _, s := range sessions {
		if p, ok := procs[s.PID]; ok {
			state, active := sessionState(p, s.SessionID, s.Cwd)
			e := entry{
				PID:        s.PID,
				State:      state,
				Name:       s.Name,
				Cwd:        shortenPath(s.Cwd),
				App:        findTerminalApp(s.PID),
				LastActive: active,
			}
			if s.StartedAt > 0 {
				e.Elapsed = time.Since(time.UnixMilli(s.StartedAt))
				e.Uptime = formatDuration(e.Elapsed)
			}
			entries = append(entries, e)
			matched[s.PID] = true
		}
	}

	for pid, p := range procs {
		if matched[pid] {
			continue
		}
		entries = append(entries, entry{
			PID:   pid,
			State: inferState(p),
			Cwd:   shortenPath(getCwdFromLsof(pid)),
			App:   findTerminalApp(pid),
		})
	}

	return entries
}

func loadSessions() []session {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	dir := filepath.Join(home, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var sessions []session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s session
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		sessions = append(sessions, s)
	}
	return sessions
}

func getClaudeProcesses() map[int]processInfo {
	out, err := exec.Command("ps", "-eo", "pid,stat,%cpu,tty,comm").Output()
	if err != nil {
		return nil
	}

	procs := make(map[int]processInfo)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if filepath.Base(fields[4]) != "claude" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		procs[pid] = processInfo{
			PID:  pid,
			Stat: fields[1],
			CPU:  cpu,
			TTY:  fields[3],
		}
	}
	return procs
}

func getCwdFromLsof(pid int) string {
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}

// jsonlPath returns the conversation log path for a session.
func jsonlPath(sessionID, cwd string) string {
	if sessionID == "" || cwd == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	encoded := strings.ReplaceAll(cwd, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded, sessionID+".jsonl")
}

// readLastLine reads the last line of a file efficiently.
func readLastLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}

	const maxRead = 256 * 1024
	size := info.Size()
	offset := max(int64(0), size-maxRead)

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxRead), maxRead)
	var last string
	for scanner.Scan() {
		last = scanner.Text()
	}
	return last
}

type jsonlEntry struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Message struct {
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

func (e jsonlEntry) contentBlocks() []contentBlock {
	var blocks []contentBlock
	if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

func (e jsonlEntry) hasToolResult() bool {
	for _, b := range e.contentBlocks() {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

func (e jsonlEntry) toolName() string {
	for _, b := range e.contentBlocks() {
		if b.Type == "tool_use" {
			return b.Name
		}
	}
	return ""
}

func parseEntry(line string) (jsonlEntry, bool) {
	var e jsonlEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return e, false
	}
	return e, true
}

// sessionState determines the display state and last-active time from the JSONL log.
func sessionState(p processInfo, sessionID, cwd string) (state, lastActive string) {
	if strings.HasPrefix(p.Stat, "R") || p.CPU > 5.0 {
		return "working", ""
	}

	path := jsonlPath(sessionID, cwd)
	if path == "" {
		return "idle", ""
	}

	if info, err := os.Stat(path); err == nil {
		lastActive = formatDuration(time.Since(info.ModTime())) + " ago"
	}

	line := readLastLine(path)
	if line == "" {
		return "idle", lastActive
	}

	var entry jsonlEntry
	if e, ok := parseEntry(line); ok {
		entry = e
	} else {
		return "idle", lastActive
	}

	switch entry.Type {
	case "system":
		return "idle", lastActive
	case "user":
		if entry.hasToolResult() {
			return "working", lastActive
		}
		return "idle", lastActive
	case "assistant":
		switch entry.Message.StopReason {
		case "tool_use":
			return "waiting", lastActive
		case "":
			return "working", lastActive
		}
	}

	return "idle", lastActive
}

func findTerminalApp(pid int) string {
	p := pid
	for p > 1 {
		out, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(p)).Output()
		if err != nil {
			break
		}
		line := strings.TrimSpace(string(out))
		if line == "" {
			break
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			break
		}
		ppid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			break
		}
		comm := strings.TrimSpace(parts[1])

		if name := extractAppName(comm); name != "" {
			return name
		}
		p = ppid
	}
	return "-"
}

func extractAppName(comm string) string {
	lower := strings.ToLower(comm)

	if strings.Contains(lower, ".app/") {
		idx := strings.Index(lower, ".app/")
		prefix := comm[:idx]
		if slash := strings.LastIndex(prefix, "/"); slash >= 0 {
			return prefix[slash+1:]
		}
		return prefix
	}

	base := filepath.Base(comm)
	switch base {
	case "tmux":
		return "tmux"
	case "screen":
		return "screen"
	case "zellij":
		return "zellij"
	}
	return ""
}

func inferState(p processInfo) string {
	if strings.HasPrefix(p.Stat, "R") {
		return "working"
	}
	if p.CPU > 5.0 {
		return "working"
	}
	return "idle"
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func shortenPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func main() {
	p := tea.NewProgram(model{}, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
