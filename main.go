package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	PID        int
	State      string
	Name       string
	Cwd        string
	App        string
	Start      string
	LastActive string
	Stats      *sessionStats
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
	labelStyle    = lipgloss.NewStyle().Faint(true)
)

// stats cache
type sessionStats struct {
	offset       int64
	model        string
	inputTokens  int64
	outputTokens int64
	cacheRead    int64
	cacheCreate  int64
	toolUses     int
	turns        int
	lastLine      string
	lastStateLine string // last assistant/user/progress line for state detection
}

func (s *sessionStats) shortModel() string {
	switch {
	case strings.Contains(s.model, "opus"):
		return "opus"
	case strings.Contains(s.model, "haiku"):
		return "haiku"
	case strings.Contains(s.model, "sonnet"):
		return "sonnet"
	case s.model == "":
		return "-"
	default:
		return s.model
	}
}

var (
	statsCache = make(map[string]*sessionStats)
	statsMu    sync.Mutex
)

type statsJsonlEntry struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Message struct {
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
		Usage      struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func updateStats(sessionID, path string) *sessionStats {
	statsMu.Lock()
	defer statsMu.Unlock()

	stats, ok := statsCache[sessionID]
	if !ok {
		stats = &sessionStats{}
		statsCache[sessionID] = stats
	}

	f, err := os.Open(path)
	if err != nil {
		return stats
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return stats
	}

	if info.Size() <= stats.offset {
		return stats
	}

	if stats.offset > 0 {
		if _, err := f.Seek(stats.offset, io.SeekStart); err != nil {
			return stats
		}
	}

	const maxBuf = 512 * 1024
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxBuf), maxBuf)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		stats.lastLine = line

		var e statsJsonlEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}

		// Track the last line relevant for state detection
		switch e.Type {
		case "assistant", "user", "progress":
			stats.lastStateLine = line
		}

		if e.Type == "assistant" {
			u := e.Message.Usage
			stats.inputTokens += u.InputTokens
			stats.outputTokens += u.OutputTokens
			stats.cacheRead += u.CacheReadInputTokens
			stats.cacheCreate += u.CacheCreationInputTokens

			if e.Message.Model != "" && e.Message.Model != "<synthetic>" {
				stats.model = e.Message.Model
			}

			var blocks []contentBlock
			if err := json.Unmarshal(e.Message.Content, &blocks); err == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" {
						stats.toolUses++
					}
				}
			}
		}

		if e.Type == "system" && e.Subtype == "turn_duration" {
			stats.turns++
		}

	}

	stats.offset = info.Size()
	return stats
}

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

	// Table columns
	cols := [...]string{"PID", "STATE", "MODEL", "CWD", "APP", "ACTIVE", "START"}
	numCols := len(cols)
	widths := make([]int, numCols)
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

	var fmtParts []string
	for _, w := range widths {
		fmtParts = append(fmtParts, fmt.Sprintf("%%-%ds", w))
	}
	fmtStr := strings.Join(fmtParts, "  ")

	colVals := make([]any, numCols)
	for i, c := range cols {
		colVals[i] = c
	}
	header := fmt.Sprintf(fmtStr, colVals...)
	b.WriteString(headerStyle.Render(padRight(header, m.width)))
	b.WriteString("\n")

	for i, e := range m.entries {
		vals := rowValues(e)
		rowVals := make([]any, numCols)
		for j, v := range vals {
			rowVals[j] = v
		}
		line := fmt.Sprintf(fmtStr, rowVals...)

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

	// Detail pane for selected session
	b.WriteString("\n")
	if m.cursor < len(m.entries) {
		e := m.entries[m.cursor]
		b.WriteString(renderDetails(e, m.width))
	}

	b.WriteString("\n")
	if m.message != "" {
		b.WriteString(m.message + "\n")
	}
	b.WriteString(dimStyle.Render("↑/↓: navigate   k: kill   q: quit"))
	b.WriteString("\n")

	return b.String()
}

func renderDetails(e entry, width int) string {
	var b strings.Builder

	label := func(l string) string { return labelStyle.Render(l) }

	s := e.Stats
	if s == nil {
		return ""
	}

	// Row 1: tokens
	b.WriteString(fmt.Sprintf("  %s %s   %s %s   %s %s   %s %s\n",
		label("input:"), formatTokens(s.inputTokens),
		label("output:"), formatTokens(s.outputTokens),
		label("cache read:"), formatTokens(s.cacheRead),
		label("cache write:"), formatTokens(s.cacheCreate),
	))

	// Row 2: activity
	b.WriteString(fmt.Sprintf("  %s %d   %s %d\n",
		label("turns:"), s.turns,
		label("tool uses:"), s.toolUses,
	))

	return b.String()
}

func rowValues(e entry) [7]string {
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	model := "-"
	if e.Stats != nil {
		model = e.Stats.shortModel()
	}
	return [7]string{
		strconv.Itoa(e.PID),
		e.State,
		model,
		dash(e.Cwd),
		dash(e.App),
		dash(e.LastActive),
		dash(e.Start),
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
			path := jsonlPath(s.SessionID, s.Cwd)
			stats := updateStats(s.SessionID, path)
			state, active := sessionStateFromStats(p, stats, path)

			e := entry{
				PID:        s.PID,
				State:      state,
				Name:       s.Name,
				Cwd:        shortenPath(s.Cwd),
				App:        findTerminalApp(s.PID),
				LastActive: active,
				Stats:      stats,
			}
			if s.StartedAt > 0 {
				e.Start = formatTimestamp(time.UnixMilli(s.StartedAt))
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

func sessionStateFromStats(p processInfo, stats *sessionStats, path string) (state, lastActive string) {
	// High CPU = definitely working
	if strings.HasPrefix(p.Stat, "R") || p.CPU > 5.0 {
		return "working", "now"
	}

	if path == "" {
		return "idle", ""
	}

	var mtime time.Time
	if info, err := os.Stat(path); err == nil {
		mtime = info.ModTime()
		if time.Since(mtime) < time.Minute {
			lastActive = "now"
		} else {
			lastActive = formatDuration(time.Since(mtime)) + " ago"
		}
	}

	// Primary signal: file modification time. When Claude is working,
	// the JSONL is being written to continuously. A fresh mtime is
	// the strongest indicator of activity.
	if !mtime.IsZero() && time.Since(mtime) < 5*time.Second {
		// File is actively being written — use last entry to
		// distinguish working vs waiting for user approval.
		if s := lastEntryState(stats.lastStateLine); s != "" {
			return s, "now"
		}
		return "working", "now"
	}

	// If the last JSONL entry indicates the session is blocked on
	// something (tool approval or a question), keep it "waiting"
	// regardless of how long ago the file was written — the user
	// simply hasn't responded yet.
	if s := lastEntryState(stats.lastStateLine); s == "waiting" {
		return "waiting", lastActive
	}

	return "idle", lastActive
}

// lastEntryState returns "working", "waiting", or "" based on the
// last JSONL line. Only used to distinguish working vs waiting when
// we already know the file is fresh.
func lastEntryState(line string) string {
	if line == "" {
		return ""
	}
	entry, ok := parseEntry(line)
	if !ok {
		return ""
	}

	switch entry.Type {
	case "assistant":
		// Still streaming (no stop_reason yet)
		if entry.Message.StopReason == "" {
			return "working"
		}
		// Tool call — check if it needs user approval
		if s := entry.toolUseState(); s != "" {
			return s
		}
		// Response ended with a question mark — Claude is asking
		// the user something without using the formal question tool.
		if entry.endsWithQuestion() {
			return "waiting"
		}
		return ""

	case "user":
		// Tool result just came back — Claude is about to respond
		if entry.hasToolResult() {
			return "working"
		}
		// User sent a message — Claude should start soon
		return "working"

	case "progress":
		return "working"
	}

	return ""
}

func loadSessions() []session {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	dir := filepath.Join(home, ".claude", "sessions")
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var sessions []session
	for _, e := range dirEntries {
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

// sanitizePath mirrors Claude Code's sanitizePath from
// utils/sessionStoragePortable.ts — replaces ALL non-alphanumeric
// characters with hyphens, not just slashes.
var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]`)

func sanitizePath(name string) string {
	return nonAlphanumeric.ReplaceAllString(name, "-")
}

func jsonlPath(sessionID, cwd string) string {
	if sessionID == "" || cwd == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	encoded := sanitizePath(cwd)
	direct := filepath.Join(home, ".claude", "projects", encoded, sessionID+".jsonl")
	if _, err := os.Stat(direct); err == nil {
		return direct
	}

	// Resumed sessions write to the original session's JSONL file.
	// Fall back to the most recently modified .jsonl in the project dir.
	dir := filepath.Join(home, ".claude", "projects", encoded)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var bestPath string
	var bestTime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(bestTime) {
			bestTime = info.ModTime()
			bestPath = filepath.Join(dir, e.Name())
		}
	}
	return bestPath
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
	Text string `json:"text,omitempty"`
}

func (e jsonlEntry) contentBlocks() []contentBlock {
	var blocks []contentBlock
	if err := json.Unmarshal(e.Message.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// toolUseState classifies what the tool_use blocks mean for session state.
// Returns "waiting" if any tool needs user interaction, "working" if all
// tools are auto-approved, or "" if no tool_use blocks found.
func (e jsonlEntry) toolUseState() string {
	hasAny := false
	for _, b := range e.contentBlocks() {
		if b.Type != "tool_use" {
			continue
		}
		hasAny = true
		if !autoApprovedTool(b.Name) {
			return "waiting"
		}
	}
	if hasAny {
		return "working"
	}
	return ""
}

// autoApprovedTool returns true for tools that never require user permission.
// These tools execute immediately without a permission prompt.
func autoApprovedTool(name string) bool {
	switch name {
	case "Read", "Glob", "Grep", "ToolSearch", "LSP",
		"ListMcpResources", "ReadMcpResource",
		"TodoWrite", "TaskCreate", "TaskUpdate", "TaskOutput",
		"TaskStop", "TaskGet", "TaskList",
		"Brief", "Agent", "SendMessage",
		"EnterPlanMode", "EnterWorktree", "ExitWorktree":
		return true
	}
	return false
}

func (e jsonlEntry) hasToolResult() bool {
	for _, b := range e.contentBlocks() {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// endsWithQuestion returns true if the last text block in the message
// ends with a question mark, indicating Claude asked the user something
// without using the formal question tool.
func (e jsonlEntry) endsWithQuestion() bool {
	var last string
	for _, b := range e.contentBlocks() {
		if b.Type == "text" && b.Text != "" {
			last = b.Text
		}
	}
	return strings.HasSuffix(strings.TrimSpace(last), "?")
}

func parseEntry(line string) (jsonlEntry, bool) {
	var e jsonlEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return e, false
	}
	return e, true
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
	if strings.HasPrefix(p.Stat, "R") || p.CPU > 5.0 {
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

func formatTimestamp(t time.Time) string {
	return formatDuration(time.Since(t)) + " ago"
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

func formatTokens(t int64) string {
	switch {
	case t >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(t)/1_000_000_000)
	case t >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(t)/1_000_000)
	case t >= 1_000:
		return fmt.Sprintf("%.1fk", float64(t)/1_000)
	case t > 0:
		return strconv.FormatInt(t, 10)
	default:
		return "0"
	}
}

func main() {
	p := tea.NewProgram(model{}, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
