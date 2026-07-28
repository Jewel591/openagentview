package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/dismiss"
	"github.com/Jewel591/openagentview/internal/tmux"
)

var benchmarkRenderedQuickLook string
var benchmarkPreviewLayout []string

type previewAdapter struct {
	messages []agent.TranscriptMessage
	status   agent.RuntimeStatus
	activity agent.Activity
	calls    int
}

func (a *previewAdapter) Name() string {
	return "codex"
}

func (a *previewAdapter) Discover(context.Context, int) ([]agent.Session, error) {
	return nil, nil
}

func (a *previewAdapter) Preview(
	context.Context,
	agent.Session,
	int,
) (agent.Transcript, error) {
	a.calls++
	return agent.Transcript{
		Messages: append([]agent.TranscriptMessage(nil), a.messages...),
		Status:   a.status,
		Activity: a.activity,
	}, nil
}

func (a *previewAdapter) ResumeCommand(agent.Session) (string, []string) {
	return "", nil
}

func (a *previewAdapter) Archive(context.Context, agent.Session) error {
	return nil
}

type fakePanes struct {
	lines   []string
	cursorX int
	cursorY int
	cursor  bool
	width   int
	err     error
	sent    []string
}

func (p *fakePanes) Capture(context.Context, string) (tmux.Screen, error) {
	return tmux.Screen{
		Lines:         append([]string(nil), p.lines...),
		CursorX:       p.cursorX,
		CursorY:       p.cursorY,
		CursorVisible: p.cursor,
		Width:         p.width,
	}, p.err
}

func (p *fakePanes) SendText(_ context.Context, pane, text string) error {
	p.sent = append(p.sent, pane+" text:"+text)
	return nil
}

func (p *fakePanes) SendKey(_ context.Context, pane, key string) error {
	p.sent = append(p.sent, pane+" key:"+key)
	return nil
}

func tmuxModel(panes *fakePanes) *Model {
	m := &Model{
		adapter: &previewAdapter{},
		panes:   panes,
		width:   120,
		height:  40,
		group:   groupStatus,
		sessions: []agent.Session{
			{
				ID:            "live",
				Agent:         "codex",
				Title:         "Session in a pane",
				CWD:           "/projects/openagentview",
				RuntimeStatus: agent.StatusRunning,
				PID:           4242,
				TmuxPane:      "%7",
				TmuxTarget:    "cw:2.0",
				RecencyAt:     time.Now(),
			},
		},
	}
	// Land the cursor on the session the way a refresh would: the first
	// column, Needs You, is empty.
	m.clampSelection()
	return m
}

func press(t *testing.T, m *Model, key tea.KeyPressMsg) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(key)
	return cmd
}

// loadMsg digs the load message out of the command Quick Look returns, which
// batches it with the opening zoom's first frame.
func loadMsg(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("Quick Look returned no command")
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg
	}
	for _, part := range batch {
		switch loaded := part().(type) {
		case paneLoadedMsg, previewLoadedMsg:
			return loaded
		}
	}
	t.Fatal("Quick Look's batch held no load message")
	return nil
}

// settleQuickLook fast-forwards past the opening zoom, so assertions see the
// settled overlay rather than an animation frame.
func settleQuickLook(m *Model) {
	m.previewOpenedAt = time.Now().Add(-2 * previewOpenDuration)
}

// A session running in a pane has a screen, and the screen shows what the
// rollout log cannot: the prompt the agent is currently blocked on.
func TestQuickLookMirrorsTheTmuxPaneOfALiveSession(t *testing.T) {
	panes := &fakePanes{lines: []string{"› approve shell command? [y/N]"}}
	m := tmuxModel(panes)

	load := m.openQuickLook()
	if !m.paneView {
		t.Fatal("Quick Look on a tmux session did not mirror its pane")
	}
	// Mirroring is only half the point: the overlay opens ready to answer the
	// agent rather than behind a mode.
	if !m.paneInput {
		t.Fatal("the mirrored pane did not open ready to type into")
	}
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	settleQuickLook(m)

	if !strings.Contains(m.View().Content, "approve shell command?") {
		t.Fatal("the overlay did not show the pane's screen")
	}
	if got := m.previewSession.TmuxTarget; !strings.Contains(m.View().Content, got) {
		t.Fatalf("the overlay did not name the pane %q", got)
	}
}

func TestQuickLookFallsBackToTheTranscriptWithoutAPane(t *testing.T) {
	m := tmuxModel(&fakePanes{})
	m.sessions[0].TmuxPane = ""
	m.sessions[0].TmuxTarget = ""

	load := m.openQuickLook()
	if m.paneView {
		t.Fatal("a session outside tmux was mirrored as a pane")
	}
	if _, ok := loadMsg(t, load).(previewLoadedMsg); !ok {
		t.Fatal("Quick Look did not fall back to reading the transcript")
	}
}

func TestTypingIntoAPaneSendsTextAndReturnSeparately(t *testing.T) {
	panes := &fakePanes{lines: []string{"waiting"}}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	for _, key := range []tea.KeyPressMsg{
		{Code: 'y', Text: "y"},
		{Code: tea.KeyEnter},
	} {
		cmd := press(t, m, key)
		if cmd == nil {
			t.Fatalf("keystroke %v was not forwarded to the pane", key.Keystroke())
		}
		if _, ok := cmd().(paneSentMsg); !ok {
			t.Fatalf("keystroke %v did not report a send", key.Keystroke())
		}
	}

	want := []string{"%7 text:y", "%7 key:Enter"}
	if strings.Join(panes.sent, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %v, want %v", panes.sent, want)
	}
}

// Esc belongs to the agent while typing — codex binds it — so leaving typing
// is a keystroke no agent TUI uses.
func TestCtrlBracketLeavesTypingWithoutClosingQuickLook(t *testing.T) {
	panes := &fakePanes{lines: []string{"waiting"}}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	// Esc goes to the agent, not to the overlay.
	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("esc was swallowed instead of reaching the agent")
	}
	cmd()
	if !m.paneInput || !m.previewOpen {
		t.Fatal("esc stopped typing instead of reaching the agent")
	}
	if len(panes.sent) != 1 || !strings.HasSuffix(panes.sent[0], "key:Escape") {
		t.Fatalf("sent %v, want an Escape for the agent", panes.sent)
	}

	press(t, m, tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})
	if m.paneInput {
		t.Fatal("ctrl+] did not stop typing")
	}
	if !m.previewOpen {
		t.Fatal("ctrl+] closed Quick Look instead of stopping typing")
	}

	press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	if !m.paneInput {
		t.Fatal("i did not start typing again")
	}
	press(t, m, tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})

	press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.previewOpen {
		t.Fatal("esc did not close Quick Look once typing had stopped")
	}
}

// Space opens and closes Quick Look everywhere else on the board, and keeps
// doing so inside a mirrored pane. The space bar is the price: a typed space
// is ctrl+space.
func TestSpaceClosesTheMirrorAndCtrlSpaceTypesOne(t *testing.T) {
	panes := &fakePanes{lines: []string{"waiting"}}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	cmd := press(t, m, tea.KeyPressMsg{Code: ' ', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+space did not reach the pane")
	}
	cmd()
	if !m.previewOpen {
		t.Fatal("ctrl+space closed Quick Look instead of typing a space")
	}
	if len(panes.sent) != 1 || !strings.HasSuffix(panes.sent[0], "text: ") {
		t.Fatalf("sent %v, want a literal space", panes.sent)
	}

	press(t, m, tea.KeyPressMsg{Code: ' ', Text: " "})
	if m.previewOpen || m.paneInput {
		t.Fatal("space did not close the mirror while typing")
	}
	if len(panes.sent) != 1 {
		t.Fatalf("sent %v, want the closing space kept out of the pane", panes.sent)
	}
}

func TestToggleSwapsBetweenPaneAndTranscript(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"live screen"}})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	// While typing, t belongs to the agent; ctrl+] hands the board's keys back.
	press(t, m, tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})

	cmd := press(t, m, tea.KeyPressMsg{Code: 't', Text: "t"})
	if m.paneView {
		t.Fatal("t did not switch to the transcript")
	}
	if _, ok := cmd().(previewLoadedMsg); !ok {
		t.Fatal("switching to the transcript did not load one")
	}

	cmd = press(t, m, tea.KeyPressMsg{Code: 't', Text: "t"})
	if !m.paneView {
		t.Fatal("t did not switch back to the pane")
	}
	if _, ok := cmd().(paneLoadedMsg); !ok {
		t.Fatal("switching back to the pane did not capture it")
	}
}

// A capture holds cells, not terminal state, so the mirrored pane brings no
// cursor of its own. While typing, this terminal lends it one — the only thing
// on screen saying where the next keystroke lands.
func TestMirrorShowsTheAgentsCursorWhileTyping(t *testing.T) {
	panes := &fakePanes{
		lines:   []string{"› ", "", "status bar"},
		cursorX: 2,
		cursorY: 0,
		cursor:  true,
		width:   36,
	}
	m := tmuxModel(panes)
	// A terminal too small for a frame, so the mirror fills it and the cursor
	// is at the pane's own coordinates.
	m.width, m.height = 36, 20
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	settleQuickLook(m)

	cursor := m.View().Cursor
	if cursor == nil {
		t.Fatal("the mirror showed no cursor while typing into the agent")
	}
	if cursor.X != 2 || cursor.Y != quickLookBodyRow {
		t.Fatalf("cursor = %d,%d, want 2,%d", cursor.X, cursor.Y, quickLookBodyRow)
	}

	// Browsing is not typing: the cursor would claim keys go somewhere they do
	// not.
	press(t, m, tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})
	if m.View().Cursor != nil {
		t.Fatal("the cursor stayed on after typing stopped")
	}
}

// A pane narrower than the terminal watching it is mirrored in a window, with
// the board still visible around it — and the borrowed cursor moves with it.
func TestNarrowPaneIsMirroredInAWindow(t *testing.T) {
	panes := &fakePanes{
		lines:   []string{"› ", "", "status bar"},
		cursorX: 2,
		cursorY: 0,
		cursor:  true,
		width:   80,
	}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	settleQuickLook(m)

	if !m.paneFloats() {
		t.Fatal("an 80-column pane filled a 120-column terminal")
	}
	width, height := m.quickLookDimensions()
	if width >= m.width || height >= m.height {
		t.Fatalf(
			"window %dx%d left no gap in a %dx%d terminal",
			width, height, m.width, m.height,
		)
	}
	// The window exists to leave a gap, not to cut the pane down to one.
	if got := m.quickLookContentWidth(); got != 80 {
		t.Fatalf("content width = %d, want the pane's own 80 columns", got)
	}

	x, y := (m.width-width)/2, (m.height-height)/2
	cursor := m.View().Cursor
	if cursor == nil {
		t.Fatal("the mirror showed no cursor while typing into the agent")
	}
	// Past the window's left border and padding, and below its top border.
	wantX, wantY := x+3+2, y+1+quickLookBodyRow
	if cursor.X != wantX || cursor.Y != wantY {
		t.Fatalf(
			"cursor = %d,%d, want %d,%d inside the window",
			cursor.X, cursor.Y, wantX, wantY,
		)
	}
}

// A pane that fills the terminal — what an agent run in a window this size
// leaves behind — still gets a window, on the tightest frame that is one: a
// border, no padding, and two of the agent's columns spent on it.
func TestPaneFillingTheTerminalGetsATightFrame(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"wide"}, width: 120})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	if !m.paneFloats() {
		t.Fatal("a 120-column pane filled a 120-column terminal edge to edge")
	}
	width, _ := m.quickLookDimensions()
	if width != m.width-2 {
		t.Fatalf("window width = %d, want the terminal less its margins", width)
	}
	if got := m.quickLookContentWidth(); got != m.width-4 {
		t.Fatalf("content width = %d, want the terminal less frame and margins", got)
	}
}

// Below a certain size the frame stops being decoration and becomes most of
// the screen, so the mirror takes the terminal whole.
func TestTinyTerminalDropsTheFrame(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"cramped"}, width: 36})
	m.width, m.height = 36, 20
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	if m.paneFloats() {
		t.Fatal("a 36-column terminal spent columns on a frame")
	}
	if got := m.quickLookContentWidth(); got != m.width {
		t.Fatalf("content width = %d, want the whole terminal", got)
	}
}

func TestMirrorHidesTheCursorTheAgentHides(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"working…"}, cursor: false})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	if m.View().Cursor != nil {
		t.Fatal("the mirror invented a cursor the pane does not show")
	}
}

// An agent waiting on an empty composer leaves its cursor below the last line
// it wrote, so the blank rows above it have to survive the trim.
func TestBlankRowsSurviveDownToTheCursor(t *testing.T) {
	lines := []string{"conversation", "", "", "", ""}
	if got := trimBlankTail(lines, 0); len(got) != 1 {
		t.Fatalf("trimBlankTail(…, 0) kept %d rows, want 1", len(got))
	}
	got := trimBlankTail(lines, 4)
	if len(got) != 4 {
		t.Fatalf("rows kept = %d, want the four down to the cursor", len(got))
	}
	if len(trimBlankTail(lines, 99)) != len(lines) {
		t.Fatal("a cursor past the capture dropped rows that exist")
	}
}

// The top and bottom rows of a scrolled mirror are replaced by scroll markers,
// so a cursor on one of them would be drawn over text that is not the pane's.
func TestCursorIsDroppedWhereScrollMarkersReplaceTheRow(t *testing.T) {
	m := tmuxModel(&fakePanes{cursor: true})
	m.paneView = true
	m.paneLines = make([]string, 100)
	m.paneCursor = tmux.Screen{CursorVisible: true, CursorY: 20}

	if row := m.cursorRow(20, 40); row != -1 {
		t.Fatalf("cursor row = %d on the scrolled-off top marker, want -1", row)
	}
	if row := m.cursorRow(10, 21); row != -1 {
		t.Fatalf("cursor row = %d on the bottom marker, want -1", row)
	}
	if row := m.cursorRow(10, 40); row != quickLookBodyRow+10 {
		t.Fatalf("cursor row = %d, want %d", row, quickLookBodyRow+10)
	}
	if row := m.cursorRow(30, 60); row != -1 {
		t.Fatalf("cursor row = %d when scrolled out of view, want -1", row)
	}
}

func TestPaneKeyNameTranslatesTheKeysAnAgentNeeds(t *testing.T) {
	cases := map[string]string{
		"enter":     "Enter",
		"backspace": "BSpace",
		"ctrl+c":    "C-c",
		"up":        "Up",
		" ":         "Space",
	}
	for stroke, want := range cases {
		got, ok := paneKeyName(stroke)
		if !ok || got != want {
			t.Fatalf("paneKeyName(%q) = %q, %v, want %q", stroke, got, ok, want)
		}
	}
	// Guessing at an unknown key is worse than dropping it: the pane holds a
	// live agent.
	if _, ok := paneKeyName("f13"); ok {
		t.Fatal("paneKeyName() invented a key for an unmapped keystroke")
	}
}

// A capture is a rendered screen, so it cannot be rewrapped to fit a narrower
// overlay. Cropping it must not spill the pane's colours across the board.
func TestPaneLinesAreCroppedWithinTheOverlay(t *testing.T) {
	wide := "\x1b[31m" + strings.Repeat("x", 400) + "\x1b[0m"
	m := tmuxModel(&fakePanes{lines: []string{wide}})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	settleQuickLook(m)

	for _, line := range m.paneBodyLines(60) {
		if width := lipgloss.Width(line); width > 60 {
			t.Fatalf("pane line width = %d, want within 60", width)
		}
	}
	view := m.View().Content
	for _, line := range strings.Split(view, "\n") {
		if width := lipgloss.Width(line); width > m.width {
			t.Fatalf("board line width = %d, want within %d", width, m.width)
		}
	}
	if !strings.Contains(m.View().Content, "cols cut off") {
		t.Fatal("the overlay did not say the pane was too wide to show")
	}
}

func TestPaneReadFailureIsShownInsteadOfAnEmptyOverlay(t *testing.T) {
	m := tmuxModel(&fakePanes{err: errors.New("can't find pane: %7")})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	settleQuickLook(m)

	view := m.View().Content
	if !strings.Contains(view, "Unable to read the pane") ||
		!strings.Contains(view, "can't find pane") {
		t.Fatal("a failed capture did not surface tmux's reason")
	}
}

// Only the polling chain schedules the next capture; a keystroke's own capture
// must not leave a second poller running behind it.
func TestKeystrokeCapturesDoNotStartASecondPoller(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"waiting"}})
	load := m.openQuickLook()
	_, polled := m.Update(loadMsg(t, load).(paneLoadedMsg))
	if polled == nil {
		t.Fatal("the first capture did not start polling")
	}

	_, follow := m.Update(paneLoadedMsg{
		sessionID:  m.previewSessionID,
		generation: m.previewGeneration,
		screen:     tmux.Screen{Lines: []string{"waiting"}},
	})
	if follow != nil {
		t.Fatal("a keystroke's capture scheduled its own poll")
	}
}

func TestResumeReturnsToASessionAlreadyRunningInAPane(t *testing.T) {
	m := tmuxModel(&fakePanes{})
	if cmd := m.resumeSelected(); cmd == nil {
		t.Fatalf("a live session in a pane could not be returned to: %q", m.status)
	}

	m.sessions[0].TmuxPane = ""
	m.sessions[0].TmuxTarget = ""
	if cmd := m.resumeSelected(); cmd != nil {
		t.Fatal("a live session outside tmux was resumed a second time")
	}
}

func TestMatchesSearchesSessionMetadata(t *testing.T) {
	session := agent.Session{
		Title:  "Fix token refresh",
		CWD:    "/projects/mono",
		Branch: "feat/auth",
	}
	for _, query := range []string{"token", "MONO", "auth"} {
		if !matches(session, query) {
			t.Fatalf("matches() did not find %q", query)
		}
	}
	if matches(session, "filmo") {
		t.Fatal("matches() returned true for unrelated query")
	}
}

func TestRelativeTime(t *testing.T) {
	if got := relativeTime(time.Now().Add(-90 * time.Minute)); got != "1h ago" {
		t.Fatalf("relativeTime() = %q, want %q", got, "1h ago")
	}
}

func TestTruncateRespectsTerminalWidth(t *testing.T) {
	got := truncate("一个很长的 session title", 10)
	if width := lipgloss.Width(got); width > 10 {
		t.Fatalf("truncate() width = %d, want <= 10", width)
	}
}

func TestRestoreSelectionKeepsSessionAcrossReordering(t *testing.T) {
	m := &Model{
		group:  groupStatus,
		column: 2,
		row:    1,
		sessions: []agent.Session{
			{ID: "newest", RuntimeStatus: agent.StatusIdle, UpdatedAt: time.Now()},
			{ID: "selected", RuntimeStatus: agent.StatusIdle, UpdatedAt: time.Now().Add(-time.Minute)},
		},
	}
	m.sessions[1].UpdatedAt = time.Now().Add(time.Minute)
	m.restoreSelection("selected")

	selected := m.selected()
	if selected == nil || selected.ID != "selected" {
		t.Fatalf("selected session = %#v, want selected", selected)
	}
}

func TestProjectColumnsGroupAndSortByWorkspace(t *testing.T) {
	now := time.Now()
	m := &Model{
		group: groupProject,
		sessions: []agent.Session{
			{ID: "mono-old", CWD: "/projects/mono", UpdatedAt: now.Add(-time.Hour)},
			{ID: "filmo", CWD: "/projects/filmo", UpdatedAt: now},
			{ID: "mono-new", CWD: "/projects/mono", UpdatedAt: now.Add(-time.Minute)},
			{ID: "archived", CWD: "/projects/hidden", UpdatedAt: now.Add(time.Hour), Archived: true},
		},
	}

	columns := m.columns()
	if len(columns) != 2 {
		t.Fatalf("project columns = %d, want 2", len(columns))
	}
	if columns[0].project != "/projects/filmo" {
		t.Fatalf("first project = %q, want filmo", columns[0].project)
	}
	if cards := m.cardsForColumn(1); len(cards) != 2 {
		t.Fatalf("mono cards = %d, want 2", len(cards))
	}
}

func TestToggleGroupResetsSelectionAndCycles(t *testing.T) {
	m := &Model{
		group:  groupStatus,
		column: 3,
		row:    4,
	}
	m.toggleGroup()

	if m.group != groupProject {
		t.Fatalf("group = %v, want groupProject", m.group)
	}
	if m.column != 0 || m.row != 0 {
		t.Fatalf("selection = %d,%d, want 0,0", m.column, m.row)
	}
	m.toggleGroup()
	if m.group != groupStatus {
		t.Fatalf("group after projects = %v, want groupStatus", m.group)
	}
}

// The two axes are independent: switching layout must not touch the grouping,
// so Projects can be read as a list and Status as a board.
func TestLayoutTogglesIndependentlyOfGrouping(t *testing.T) {
	m := &Model{group: groupProject}
	_, _ = m.handleKey(tea.KeyPressMsg{Code: 'v', Text: "v"})
	if m.layout != layoutList {
		t.Fatalf("layout after v = %v, want layoutList", m.layout)
	}
	if m.group != groupProject {
		t.Fatalf("group after v = %v, want groupProject untouched", m.group)
	}
	m.toggleLayout()
	if m.layout != layoutKanban {
		t.Fatalf("layout after second toggle = %v, want layoutKanban", m.layout)
	}
}

func listModel() *Model {
	now := time.Now()
	return &Model{
		layout: layoutList,
		width:  120,
		height: 36,
		sessions: []agent.Session{
			{ID: "needs", RuntimeStatus: agent.StatusNeedsYou, RecencyAt: now, UpdatedAt: now},
			{ID: "running", RuntimeStatus: agent.StatusRunning, RecencyAt: now, UpdatedAt: now},
			{ID: "idle", RuntimeStatus: agent.StatusIdle, RecencyAt: now, UpdatedAt: now},
		},
	}
}

// The group that is waiting on a person leads, in both layouts.
func TestListModeGroupsNeedsYouFirst(t *testing.T) {
	m := listModel()
	columns := m.columns()
	if columns[0].status != agent.StatusNeedsYou {
		t.Fatalf("first group = %v, want needs-you", columns[0].status)
	}
	cards := m.cardsForColumn(0)
	if len(cards) != 1 || cards[0].ID != "needs" {
		t.Fatalf("first group cards = %#v, want the needs-you session", cards)
	}
}

// Error is a fault of the machine, not a stage of an agent's life: the group
// exists only while a session log cannot be read. Archived sessions have no
// group at all — archiving is asking for a session to be out of sight.
func TestErrorAndArchivedGroupsAreNotStandingColumns(t *testing.T) {
	now := time.Now()
	m := &Model{
		group: groupStatus,
		sessions: []agent.Session{
			{ID: "running", RuntimeStatus: agent.StatusRunning, RecencyAt: now},
			{ID: "archived", RuntimeStatus: agent.StatusArchived, Archived: true, RecencyAt: now, UpdatedAt: now},
		},
	}
	for _, column := range m.columns() {
		if column.status == agent.StatusError || column.status == agent.StatusArchived {
			t.Fatalf("board grew a %v column with nothing to put in it", column.status)
		}
	}

	m.sessions = append(m.sessions, agent.Session{
		ID: "broken", RuntimeStatus: agent.StatusError, RecencyAt: now, UpdatedAt: now,
	})
	columns := m.columns()
	last := columns[len(columns)-1]
	if last.status != agent.StatusError {
		t.Fatalf("last column = %v, want the error group once one exists", last.status)
	}
	if cards := m.cardsForColumn(len(columns) - 1); len(cards) != 1 || cards[0].ID != "broken" {
		t.Fatalf("error cards = %#v, want the unreadable session", cards)
	}
}

func TestListModeMovesAcrossGroups(t *testing.T) {
	m := listModel()

	m.moveRow(1)
	if selected := m.selected(); selected == nil || selected.ID != "running" {
		t.Fatalf("down from needs-you selected %#v, want running", selected)
	}

	m.moveRow(1)
	if selected := m.selected(); selected == nil || selected.ID != "idle" {
		t.Fatalf("down from running selected %#v, want idle", selected)
	}

	// The last row wraps to the top of the list rather than to the top of its
	// own group.
	m.moveRow(1)
	if selected := m.selected(); selected == nil || selected.ID != "needs" {
		t.Fatalf("down from the last row selected %#v, want needs", selected)
	}

	m.moveRow(-1)
	if selected := m.selected(); selected == nil || selected.ID != "idle" {
		t.Fatalf("up from the first row selected %#v, want idle", selected)
	}
}

func TestListModeRowsFitTheTerminal(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		m := listModel()
		m.width = width
		for _, line := range strings.Split(m.renderBoard(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d rendered a %d-cell line: %q", width, got, line)
			}
		}
	}
}

// A session whose title is its own first prompt has nothing to say twice, so
// the description falls back to where the session is.
func TestListDescriptionFallsBackToLocation(t *testing.T) {
	session := agent.Session{
		Agent:   "codex",
		Title:   "Fix the parser",
		Preview: "Fix the parser",
		CWD:     "/projects/mono",
		Branch:  "fix/parser",
	}
	if got := listDescription(session); got != "codex · mono · fix/parser" {
		t.Fatalf("description = %q, want the session's location", got)
	}
	session.Preview = "Fix the parser, then add a regression test"
	if got := listDescription(session); got != session.Preview {
		t.Fatalf("description = %q, want the prompt", got)
	}
}

func TestShortcutHelpUsesResponsiveHeight(t *testing.T) {
	m := &Model{helpOpen: true, width: 80}
	if got := m.footerHeight(); got != 6 {
		t.Fatalf("narrow footer height = %d, want 6", got)
	}
	m.width = 140
	if got := m.footerHeight(); got != 5 {
		t.Fatalf("wide footer height = %d, want 5", got)
	}
}

func TestMoveColumnSkipsEmptyStatusColumns(t *testing.T) {
	now := time.Now()
	m := &Model{
		group:  groupStatus,
		column: 0,
		sessions: []agent.Session{
			{ID: "running", RuntimeStatus: agent.StatusRunning, RecencyAt: now},
			{ID: "idle", RuntimeStatus: agent.StatusIdle, RecencyAt: now},
			{ID: "archived", RuntimeStatus: agent.StatusArchived, Archived: true, RecencyAt: now},
		},
	}

	// Needs You is empty, so moving right from it skips straight to Running.
	m.moveColumn(1)
	if m.column != 1 {
		t.Fatalf("right from Needs You selected column %d, want Running column 1", m.column)
	}

	m.moveColumn(1)
	if m.column != 2 {
		t.Fatalf("right from Running selected column %d, want Idle column 2", m.column)
	}

	m.moveColumn(-1)
	if m.column != 1 {
		t.Fatalf("left from Idle selected column %d, want Running column 1", m.column)
	}

	m.moveColumn(-1)
	if m.column != 2 {
		t.Fatalf("left with wrap selected column %d, want Idle column 2", m.column)
	}
}

func TestMoveColumnStaysPutWhenEveryColumnIsEmpty(t *testing.T) {
	m := &Model{group: groupStatus, column: 1}
	m.moveColumn(1)
	if m.column != 1 {
		t.Fatalf("empty board selected column %d, want 1", m.column)
	}
}

func TestDefaultBoardOnlyShowsRecentSessions(t *testing.T) {
	now := time.Now()
	m := &Model{
		group: groupStatus,
		sessions: []agent.Session{
			{ID: "recent", RuntimeStatus: agent.StatusIdle, RecencyAt: now.Add(-time.Hour)},
			{ID: "old", RuntimeStatus: agent.StatusIdle, RecencyAt: now.Add(-48 * time.Hour)},
			{ID: "old-running", RuntimeStatus: agent.StatusRunning, RecencyAt: now.Add(-48 * time.Hour)},
		},
	}

	idle := m.cardsForColumn(2)
	if len(idle) != 1 || idle[0].ID != "recent" {
		t.Fatalf("idle sessions = %#v, want only recent", idle)
	}
	running := m.cardsForColumn(1)
	if len(running) != 1 || running[0].ID != "old-running" {
		t.Fatalf("running sessions = %#v, want old live session", running)
	}
}

// Confirming a search resets the selection to the first column, which is
// Needs You — usually empty. The cursor has to land on a session that exists,
// otherwise space and enter silently do nothing.
func TestConfirmingSearchSelectsASessionWhenRunningIsEmpty(t *testing.T) {
	m := &Model{
		group:     groupStatus,
		searching: true,
		query:     "legacy",
		sessions: []agent.Session{
			{
				ID:            "old",
				Title:         "Legacy migration",
				RuntimeStatus: agent.StatusIdle,
				RecencyAt:     time.Now(),
			},
		},
	}

	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	selected := m.selected()
	if selected == nil || selected.ID != "old" {
		t.Fatalf("selection after search = %#v, want the matching session", selected)
	}
}

func TestClearingSearchSelectsASessionWhenRunningIsEmpty(t *testing.T) {
	m := &Model{
		group:  groupStatus,
		query:  "legacy",
		column: 2,
		sessions: []agent.Session{
			{ID: "idle", RuntimeStatus: agent.StatusIdle, RecencyAt: time.Now()},
		},
	}

	_, _ = m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})

	if selected := m.selected(); selected == nil {
		t.Fatal("clearing the search left the cursor on an empty column")
	}
}

func TestSearchIncludesMatchingHistoryOutsideRecentWindow(t *testing.T) {
	m := &Model{
		group: groupStatus,
		query: "legacy",
		sessions: []agent.Session{
			{
				ID:            "old",
				Title:         "Legacy migration",
				RuntimeStatus: agent.StatusIdle,
				RecencyAt:     time.Now().Add(-30 * 24 * time.Hour),
			},
		},
	}

	idle := m.cardsForColumn(2)
	if len(idle) != 1 || idle[0].ID != "old" {
		t.Fatalf("search results = %#v, want matching history", idle)
	}
}

func benchmarkSession() agent.Session {
	return agent.Session{
		ID:    "session",
		Agent: "codex",
		// Real session titles are the first line of a prompt: long, and
		// usually CJK for this board's users.
		Title:         strings.Repeat("请审查当前分支相对 main 的改动 ", 6),
		CWD:           "/projects/openagentview",
		RuntimeStatus: agent.StatusRunning,
	}
}

func BenchmarkQuickLookScroll(b *testing.B) {
	messages := make([]agent.TranscriptMessage, 16)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role:      agent.RoleAgent,
			Text:      strings.Repeat("Long preview content 中文内容 ", 400),
			Timestamp: time.Now(),
		}
	}
	m := &Model{
		width:             240,
		height:            60,
		group:             groupStatus,
		previewOpen:       true,
		previewSessionID:  "session",
		previewSession:    benchmarkSession(),
		previewMessages:   messages,
		previewScrollBack: 0,
		sessions: []agent.Session{
			{
				ID:            "session",
				Agent:         "codex",
				Title:         "Preview benchmark",
				CWD:           "/projects/openagentview",
				RuntimeStatus: agent.StatusRunning,
			},
		},
	}
	m.rebuildPreviewLayout()
	base := strings.Repeat(strings.Repeat(" ", 239)+"\n", 59)
	m.setPreviewBase(base)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.previewScrollBack = i % 100
		benchmarkRenderedQuickLook = m.renderQuickLook(base)
	}
}

func BenchmarkQuickLookFullLayout(b *testing.B) {
	messages := make([]agent.TranscriptMessage, 16)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role:      agent.RoleAgent,
			Text:      strings.Repeat("Long preview content 中文内容 ", 400),
			Timestamp: time.Now(),
		}
	}
	m := &Model{
		previewMessages: messages,
	}
	session := agent.Session{Agent: "codex"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Measure the cold pass: the layout a freshly opened overlay pays for.
		m.previewWrapped = nil
		benchmarkPreviewLayout = m.buildPreviewLines(session, 174)
	}
}

func BenchmarkQuickLookFullViewScroll(b *testing.B) {
	now := time.Now()
	sessions := make([]agent.Session, 50)
	statuses := []agent.RuntimeStatus{
		agent.StatusRunning,
		agent.StatusNeedsYou,
		agent.StatusIdle,
		agent.StatusError,
		agent.StatusArchived,
	}
	for i := range sessions {
		sessions[i] = agent.Session{
			ID:            "session-" + strconv.Itoa(i),
			Agent:         "codex",
			Title:         strings.Repeat("真实看板会话标题 ", 4),
			CWD:           "/projects/openagentview",
			Branch:        "feat/quick-look-performance",
			RuntimeStatus: statuses[i%len(statuses)],
			UpdatedAt:     now.Add(-time.Duration(i) * time.Minute),
			RecencyAt:     now,
		}
	}
	sessions[0].ID = "session"
	sessions[0].RuntimeStatus = agent.StatusRunning
	messages := make([]agent.TranscriptMessage, 16)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role:      agent.RoleAgent,
			Text:      strings.Repeat("Long preview content 中文内容 ", 400),
			Timestamp: now,
		}
	}
	m := &Model{
		width:            240,
		height:           60,
		group:            groupStatus,
		sessions:         sessions,
		previewOpen:      true,
		previewSessionID: "session",
		previewSession:   sessions[0],
		previewMessages:  messages,
	}
	m.rebuildPreviewLayout()
	m.setPreviewBase(m.renderBase())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.previewScrollBack = i % 100
		benchmarkRenderedQuickLook = m.View().Content
	}
}

func TestOverlayANSIPreservesBaseOutsideOverlay(t *testing.T) {
	base := "abcdefghij\nklmnopqrst\nuvwxyzABCD"
	overlay := "XX\nYY"

	got := overlayANSI(base, overlay, 3, 1, 10, 3)
	want := "abcdefghij\nklmXXpqrst\nuvwYYzABCD"
	if got != want {
		t.Fatalf("overlayANSI() = %q, want %q", got, want)
	}
}

func TestOverlayANSIHandlesStyledWideCharacters(t *testing.T) {
	base := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ffffff")).
		Render("一个很宽的底层画面")
	overlay := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#000000")).
		Background(lipgloss.Color("#ffffff")).
		Width(4).
		Render("预览")

	got := overlayANSI(base, overlay, 4, 0, 16, 1)
	if width := lipgloss.Width(got); width != 16 {
		t.Fatalf("overlayANSI() width = %d, want 16", width)
	}
	if !strings.Contains(got, "预览") {
		t.Fatalf("overlayANSI() = %q, want styled overlay content", got)
	}
}

func TestPreviewBackdropExtendsShortBaseToFitOverlay(t *testing.T) {
	backdrop := newPreviewBackdrop("base", 0, 0, 4, 3, 4, 3)
	got := backdrop.compose("1111\n2222\n3333")
	want := "1111\n2222\n3333"
	if got != want {
		t.Fatalf("previewBackdrop.compose() = %q, want %q", got, want)
	}
}

func TestQuickLookKeepsPollingSessionThatWasIdleWhenOpened(t *testing.T) {
	adapter := &previewAdapter{
		messages: []agent.TranscriptMessage{
			{Role: agent.RoleAgent, Text: "done"},
		},
		status: agent.StatusIdle,
	}
	m := &Model{
		adapter: adapter,
		width:   120,
		height:  40,
		column:  2,
		sessions: []agent.Session{
			{
				ID:            "idle",
				Agent:         "codex",
				Title:         "Idle session",
				CWD:           "/projects/openagentview",
				RuntimeStatus: agent.StatusIdle,
				RecencyAt:     time.Now(),
			},
		},
	}

	load := m.openQuickLook()
	_, refreshCmd := m.Update(loadMsg(t, load).(previewLoadedMsg))
	if refreshCmd == nil {
		t.Fatal("idle Quick Look stopped polling, so later work would never appear")
	}
	if line := m.previewActivityLine(); line != "" {
		t.Fatalf("idle activity line = %q, want empty", line)
	}

	// The session starts working while the overlay is open, which the board's
	// paused discovery scan cannot report.
	adapter.status = agent.StatusRunning
	adapter.activity = agent.Activity{Label: "calling shell", At: time.Now()}
	adapter.messages = append(adapter.messages, agent.TranscriptMessage{
		Role: agent.RoleUser,
		Text: "one more thing",
	})
	_, reloadCmd := m.Update(previewRefreshMsg{generation: m.previewGeneration})
	if reloadCmd == nil {
		t.Fatal("scheduled refresh did not request the latest transcript")
	}
	_, _ = m.Update(reloadCmd().(previewLoadedMsg))

	if got := m.previewMessages[len(m.previewMessages)-1].Text; got != "one more thing" {
		t.Fatalf("last message = %q, want the message written after opening", got)
	}
	if !m.previewLive() {
		t.Fatal("Quick Look did not notice the session started working")
	}
	line := m.previewActivityLine()
	if !strings.HasPrefix(line, "● working · calling shell · ") {
		t.Fatalf("activity line = %q, want a working label with an age", line)
	}
}

func TestQuickLookPrefersTranscriptStatusOverStaleBoardStatus(t *testing.T) {
	m := &Model{previewStatus: agent.StatusIdle}
	board := agent.Session{RuntimeStatus: agent.StatusRunning}
	if got := m.previewDisplayStatus(board); got != agent.StatusIdle {
		t.Fatalf("display status = %q, want the transcript's %q", got, agent.StatusIdle)
	}

	archived := agent.Session{RuntimeStatus: agent.StatusArchived}
	if got := m.previewDisplayStatus(archived); got != agent.StatusArchived {
		t.Fatalf("display status = %q, want %q", got, agent.StatusArchived)
	}
}

func TestRunningQuickLookReloadsAndRejectsStaleResults(t *testing.T) {
	adapter := &previewAdapter{
		messages: []agent.TranscriptMessage{
			{Role: agent.RoleAgent, Text: "first"},
		},
		status: agent.StatusRunning,
	}
	m := &Model{
		adapter: adapter,
		width:   80,
		height:  24,
		sessions: []agent.Session{
			{
				ID:            "running",
				Agent:         "codex",
				Title:         "Running session",
				CWD:           "/projects/openagentview",
				RuntimeStatus: agent.StatusRunning,
			},
		},
	}

	m.clampSelection()
	firstLoad := m.openQuickLook()
	firstResult := loadMsg(t, firstLoad).(previewLoadedMsg)
	_, _ = m.Update(firstResult)
	firstGeneration := m.previewGeneration

	m.previewOpen = false
	adapter.messages = append(adapter.messages, agent.TranscriptMessage{
		Role: agent.RoleAgent,
		Text: "second",
	})
	secondLoad := m.openQuickLook()
	if m.previewGeneration == firstGeneration {
		t.Fatal("reopening Quick Look did not start a new load generation")
	}

	_, _ = m.Update(firstResult)
	if !m.previewLoading {
		t.Fatal("stale preview result replaced the current load")
	}

	secondResult := loadMsg(t, secondLoad).(previewLoadedMsg)
	_, refreshCmd := m.Update(secondResult)
	if refreshCmd == nil {
		t.Fatal("running Quick Look did not schedule live refresh")
	}
	if got := m.previewMessages[len(m.previewMessages)-1].Text; got != "second" {
		t.Fatalf("reopened Quick Look last message = %q, want second", got)
	}

	m.previewScrollBack = 2
	oldStart := max(
		0,
		len(m.previewLayout)-m.quickLookBodyHeight()-m.previewScrollBack,
	)
	thirdText := strings.Repeat("third message ", 40)
	adapter.messages = append(adapter.messages, agent.TranscriptMessage{
		Role: agent.RoleAgent,
		Text: thirdText,
	})
	_, reloadCmd := m.Update(previewRefreshMsg{
		generation: m.previewGeneration,
	})
	if reloadCmd == nil {
		t.Fatal("live refresh did not request the latest transcript")
	}
	thirdResult := reloadCmd().(previewLoadedMsg)
	_, _ = m.Update(thirdResult)
	if got := m.previewMessages[len(m.previewMessages)-1].Text; got != thirdText {
		t.Fatalf("live Quick Look last message = %q, want new message", got)
	}
	newStart := max(
		0,
		len(m.previewLayout)-m.quickLookBodyHeight()-m.previewScrollBack,
	)
	if newStart != oldStart {
		t.Fatalf(
			"live refresh moved scrolled viewport from line %d to %d",
			oldStart,
			newStart,
		)
	}
	if adapter.calls != 3 {
		t.Fatalf("Preview() calls = %d, want 3", adapter.calls)
	}
}

// A live session only ever grows its newest message, which is the layout pass
// the overlay runs once a second.
func BenchmarkQuickLookStreamingLayout(b *testing.B) {
	messages := make([]agent.TranscriptMessage, 16)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role:      agent.RoleAgent,
			Text:      strings.Repeat("Long preview content 中文内容 ", 400),
			Timestamp: time.Now(),
		}
	}
	m := &Model{previewMessages: messages}
	session := agent.Session{Agent: "codex"}
	m.buildPreviewLines(session, 174)
	tail := messages[len(messages)-1].Text

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messages[len(messages)-1].Text = tail + strconv.Itoa(i)
		benchmarkPreviewLayout = m.buildPreviewLines(session, 174)
	}
}

func TestPreviewLayoutFollowsAMessageThatWasEdited(t *testing.T) {
	m := &Model{
		previewOpen:      true,
		previewSessionID: "s",
		previewSession:   agent.Session{ID: "s", Agent: "codex"},
		width:            120,
		height:           40,
	}
	m.previewMessages = []agent.TranscriptMessage{{Role: agent.RoleAgent, Text: "partial"}}
	m.rebuildPreviewLayout()

	m.previewMessages = []agent.TranscriptMessage{
		{Role: agent.RoleAgent, Text: "partial reply, now finished"},
	}
	m.rebuildPreviewLayout()

	joined := strings.Join(m.previewLayout, "\n")
	if !strings.Contains(joined, "partial reply, now finished") {
		t.Fatalf("layout = %q, want the grown message", joined)
	}
}

func TestPreviewLayoutRewrapsWhenTheOverlayResizes(t *testing.T) {
	text := strings.Repeat("wrap me ", 40)
	m := &Model{
		previewOpen:      true,
		previewSessionID: "s",
		previewSession:   agent.Session{ID: "s", Agent: "codex"},
		previewMessages:  []agent.TranscriptMessage{{Role: agent.RoleAgent, Text: text}},
		width:            200,
		height:           40,
	}
	m.rebuildPreviewLayout()
	wide := len(m.previewLayout)

	m.width = 60
	m.rebuildPreviewLayout()

	if len(m.previewLayout) <= wide {
		t.Fatalf("lines at width 60 = %d, want more than %d at width 200",
			len(m.previewLayout), wide)
	}
}

func TestTruncateFitsWideCharactersInsideTheWidth(t *testing.T) {
	cases := []struct {
		value string
		width int
	}{
		{"short", 20},
		{"exactlyten", 10},
		{strings.Repeat("真实看板会话标题", 6), 30},
		{strings.Repeat("mixed 中文 and latin ", 8), 41},
		{"one\ntwo", 5},
		{"中", 1},
		{"", 4},
	}
	for _, c := range cases {
		got := truncate(c.value, c.width)
		if width := lipgloss.Width(got); width > c.width {
			t.Fatalf("truncate(%q, %d) = %q (width %d), want within %d",
				c.value, c.width, got, width, c.width)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("truncate(%q, %d) kept a newline", c.value, c.width)
		}
	}
}

func BenchmarkTruncateLongCJKTitle(b *testing.B) {
	title := strings.Repeat("请审查当前分支相对 main 的改动 ", 12)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkRenderedQuickLook = truncate(title, 120)
	}
}

// The overlay is a fixed-size window: a short conversation must not float the
// footer up into the middle of it.
func TestQuickLookKeepsTheSameHeightWhateverTheConversation(t *testing.T) {
	base := strings.Repeat(strings.Repeat(" ", 119)+"\n", 39)
	heightFor := func(messages []agent.TranscriptMessage) int {
		m := &Model{
			width:            120,
			height:           40,
			group:            groupStatus,
			previewOpen:      true,
			previewSessionID: "session",
			previewSession:   benchmarkSession(),
			previewMessages:  messages,
		}
		m.rebuildPreviewLayout()
		m.setPreviewBase(base)
		return lipgloss.Height(m.renderQuickLook(base))
	}

	short := heightFor([]agent.TranscriptMessage{{Role: agent.RoleAgent, Text: "ok"}})
	long := heightFor([]agent.TranscriptMessage{{
		Role: agent.RoleAgent,
		Text: strings.Repeat("a long reply that wraps many times ", 200),
	}})
	if short != long {
		t.Fatalf("overlay height = %d for a short reply and %d for a long one",
			short, long)
	}
}

// A wheel pointed at Quick Look is a request to read the history — even while
// typing into a mirrored pane, where the arrow keys it would otherwise become
// walk the agent's input history instead.
func TestWheelScrollsQuickLook(t *testing.T) {
	m := &Model{previewOpen: true, paneView: true, paneInput: true}

	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.previewScrollBack != wheelScrollLines {
		t.Fatalf("scrollback after wheel up = %d, want %d",
			m.previewScrollBack, wheelScrollLines)
	}

	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.previewScrollBack != 0 {
		t.Fatalf("scrollback after wheel down past the end = %d, want 0",
			m.previewScrollBack)
	}
}

func TestWheelIsIgnoredOnTheBoard(t *testing.T) {
	m := &Model{}
	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.previewScrollBack != 0 || m.previewOpen {
		t.Fatal("a wheel event on the board changed preview state")
	}
}

// Quick Look grows out of the card it was opened on, and settles once the
// opening beat has passed.
func TestQuickLookZoomsOutOfTheSelectedCard(t *testing.T) {
	now := time.Now()
	m := &Model{
		group:  groupStatus,
		width:  120,
		height: 40,
		sessions: []agent.Session{{
			ID:            "s",
			Title:         "Session under review",
			RuntimeStatus: agent.StatusNeedsYou,
			RecencyAt:     now,
			UpdatedAt:     now,
			CWD:           "/projects/mono",
		}},
	}

	press(t, m, tea.KeyPressMsg{Code: ' ', Text: " "})

	if !m.previewOpen || !m.previewAnimating {
		t.Fatalf("space opened previewOpen=%v animating=%v, want both",
			m.previewOpen, m.previewAnimating)
	}
	if m.previewAnimFrom.width <= 0 || m.previewAnimFrom.height <= 0 {
		t.Fatalf("zoom starts from %+v, want the selected card's rectangle",
			m.previewAnimFrom)
	}

	// Past the opening beat the overlay must settle into the ordinary render.
	m.previewOpenedAt = time.Now().Add(-2 * previewOpenDuration)
	_ = m.renderQuickLook(m.previewBase)
	if m.previewAnimating {
		t.Fatal("the zoom kept running after its duration had passed")
	}
}

func TestZoomFrameEndpoints(t *testing.T) {
	from := screenRect{x: 3, y: 5, width: 20, height: 5}
	to := screenRect{x: 10, y: 2, width: 100, height: 30}
	if got := zoomFrame(from, to, 0); got != from {
		t.Fatalf("zoomFrame(0) = %+v, want the card %+v", got, from)
	}
	if got := zoomFrame(from, to, 1); got != to {
		t.Fatalf("zoomFrame(1) = %+v, want the overlay %+v", got, to)
	}
}

// The zoom's first frame must sit exactly on the selected card — corners and
// all — or the effect reads as a box appearing near the card, not out of it.
func TestOpeningFrameSitsExactlyOnTheCard(t *testing.T) {
	m := &Model{width: 120, height: 40, previewOpen: true, previewAnimating: true}
	card := screenRect{x: 25, y: 7, width: 22, height: 5}
	m.previewAnimFrom = card
	base := strings.TrimSuffix(
		strings.Repeat(strings.Repeat(".", 120)+"\n", 40), "\n",
	)

	frame := m.renderQuickLookOpening(base, agent.Session{Title: "Session"}, 0)

	lines := strings.Split(frame, "\n")
	top := []rune(ansi.Strip(lines[card.y]))
	bottom := []rune(ansi.Strip(lines[card.y+card.height-1]))
	if top[card.x] != '╭' || top[card.x+card.width-1] != '╮' {
		t.Fatalf("top border corners = %q %q at the card's edges, want ╭ ╮",
			top[card.x], top[card.x+card.width-1])
	}
	if bottom[card.x] != '╰' || bottom[card.x+card.width-1] != '╯' {
		t.Fatalf("bottom border corners = %q %q, want ╰ ╯",
			bottom[card.x], bottom[card.x+card.width-1])
	}
}

// Toggling pane/transcript mid-zoom bumps the content generation; the zoom
// rides its own generation and must keep animating rather than freeze.
func TestZoomSurvivesAContentGenerationBump(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"waiting"}})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	press(t, m, tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})
	press(t, m, tea.KeyPressMsg{Code: 't', Text: "t"})

	m.previewOpenedAt = time.Now()
	_, tick := m.Update(previewAnimMsg{generation: m.previewAnimGeneration})
	if tick == nil {
		t.Fatal("the zoom's next frame was dropped after the content generation moved on")
	}
}

// Everything below the header takes its rows from boardTopRow, so the header
// must stay one content line at any width instead of wrapping.
func TestHeaderNeverWraps(t *testing.T) {
	for _, width := range []int{40, 60, 75, 120} {
		m := &Model{width: width, height: 30, query: "a fairly long filter"}
		header := m.renderHeader()
		if got := lipgloss.Height(header); got != 2 {
			t.Fatalf("width %d header height = %d, want 2 (line + border)", width, got)
		}
		for _, line := range strings.Split(header, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Fatalf("width %d header rendered a %d-cell line", width, got)
			}
		}
	}
}

// A List row is one cell tall — too thin for a bordered frame — so the zoom's
// first frame is the smallest honest box: three rows with the row at centre.
func TestOpeningFrameHugsAListRow(t *testing.T) {
	m := listModel()
	_ = m.renderBoard()
	if m.selectedRect.height != 1 {
		t.Fatalf("list selectedRect = %+v, want a one-row rectangle", m.selectedRect)
	}

	row := m.selectedRect
	m.previewOpen = true
	m.previewAnimating = true
	m.previewAnimFrom = row
	base := strings.TrimSuffix(
		strings.Repeat(strings.Repeat(".", m.width)+"\n", m.height), "\n",
	)

	frame := m.renderQuickLookOpening(base, agent.Session{Title: "Session"}, 0)

	lines := strings.Split(frame, "\n")
	top := []rune(ansi.Strip(lines[row.y-1]))
	bottom := []rune(ansi.Strip(lines[row.y+1]))
	if top[row.x] != '╭' || top[row.x+row.width-1] != '╮' {
		t.Fatalf("top border corners = %q %q above the row, want ╭ ╮",
			top[row.x], top[row.x+row.width-1])
	}
	if bottom[row.x] != '╰' || bottom[row.x+row.width-1] != '╯' {
		t.Fatalf("bottom border corners = %q %q below the row, want ╰ ╯",
			bottom[row.x], bottom[row.x+row.width-1])
	}
}

// The composer is the board's standing input: describe a task, and it starts
// as a fresh agent in a tmux session of its own.

type fakeStarter struct {
	name    string
	dir     string
	command []string
	err     error
}

func (s *fakeStarter) NewSession(
	_ context.Context,
	name, dir string,
	command []string,
) (string, error) {
	s.name, s.dir, s.command = name, dir, command
	if s.err != nil {
		return "", s.err
	}
	return name, nil
}

func composerModel(starter *fakeStarter) *Model {
	command := func(cli string) func(string) (string, []string) {
		return func(prompt string) (string, []string) {
			return cli, []string{prompt}
		}
	}
	return &Model{
		adapter: &previewAdapter{},
		starter: starter,
		workdir: "/projects/openagentview",
		launchers: []Launcher{
			{Agent: "claude", Command: command("claude")},
			{Agent: "codex", Command: command("codex")},
		},
		width:  120,
		height: 40,
	}
}

func typeText(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		press(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestComposerStartsTheDescribedTaskInATmuxSession(t *testing.T) {
	starter := &fakeStarter{}
	m := composerModel(starter)

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if !m.composing {
		t.Fatal("n did not focus the composer")
	}
	// The composer owns the keyboard: "q" is text here, not the quit key.
	typeText(t, m, "quick fix: login")
	if m.composeText != "quick fix: login" {
		t.Fatalf("composeText = %q, want the typed task", m.composeText)
	}

	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not start the session")
	}
	if m.composing || m.composeText != "" {
		t.Fatal("starting a session did not put the composer down")
	}
	msg, ok := cmd().(sessionStartedMsg)
	if !ok {
		t.Fatalf("enter produced %T, want sessionStartedMsg", cmd())
	}
	if msg.err != nil || msg.agent != "claude" {
		t.Fatalf("started %q with err %v, want claude", msg.agent, msg.err)
	}
	if strings.Join(starter.command, " ") != "claude quick fix: login" {
		t.Fatalf("command = %v, want the prompt as one claude argument",
			starter.command)
	}
	if starter.dir != "/projects/openagentview" {
		t.Fatalf("dir = %q, want the board's own working directory", starter.dir)
	}
	if starter.name != "quick-fix-login" {
		t.Fatalf("session name = %q, want the task made addressable", starter.name)
	}

	_, refresh := m.Update(msg)
	if refresh == nil {
		t.Fatal("a started session did not trigger a refresh")
	}
	if !strings.Contains(m.status, "claude") {
		t.Fatalf("status = %q, want it to name the started agent", m.status)
	}
}

func TestComposerCyclesAgentsAndReportsFailure(t *testing.T) {
	starter := &fakeStarter{err: errors.New("no tmux binary")}
	m := composerModel(starter)

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.launchers[m.composeAgent].Agent != "codex" {
		t.Fatalf("tab selected %q, want the next agent", m.launchers[m.composeAgent].Agent)
	}
	typeText(t, m, "task")
	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	msg := cmd().(sessionStartedMsg)
	if msg.agent != "codex" {
		t.Fatalf("started %q, want the cycled agent", msg.agent)
	}
	_, _ = m.Update(msg)
	if !strings.Contains(m.status, "no tmux binary") {
		t.Fatalf("status = %q, want the failure surfaced", m.status)
	}
	// A failed start hands the draft back rather than losing it.
	if m.composeText != "task" {
		t.Fatalf("composeText = %q, want the draft restored after failure",
			m.composeText)
	}

	// Unless a newer draft is already being written over it.
	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	press(t, m, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	typeText(t, m, "newer draft")
	_, _ = m.Update(msg)
	if m.composeText != "newer draft" {
		t.Fatalf("composeText = %q, want the newer draft kept", m.composeText)
	}
}

func TestComposerPutsItselfDownWithoutStartingAnything(t *testing.T) {
	starter := &fakeStarter{}
	m := composerModel(starter)

	// Enter on an empty composer collapses it rather than starting a session.
	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("enter on an empty composer started something")
	}
	if m.composing {
		t.Fatal("enter on an empty composer did not put it down")
	}

	// Esc keeps the draft: putting a task down is not throwing it away.
	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "draft")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.composing || m.composeText != "draft" {
		t.Fatalf("esc lost the draft: composing=%v text=%q", m.composing, m.composeText)
	}

	// A whitespace-only task is no task.
	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	press(t, m, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	typeText(t, m, "   ")
	if cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("a blank task started a session")
	}
	if starter.command != nil {
		t.Fatal("the starter was reached without a task")
	}
}

func TestComposerIsAbsentWithoutAnAgentToLaunch(t *testing.T) {
	m := composerModel(&fakeStarter{})
	m.launchers = nil

	if m.composerHeight() != 0 || m.renderComposer() != "" {
		t.Fatal("a board with nothing to launch still drew the composer")
	}
	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.composing {
		t.Fatal("n focused a composer that cannot start anything")
	}
}

func TestComposerBarShowsThePlaceAndTheAgent(t *testing.T) {
	m := composerModel(&fakeStarter{})
	bar := ansi.Strip(m.renderComposer())
	if !strings.Contains(bar, "describe a task") || !strings.Contains(bar, "claude") {
		t.Fatalf("idle bar = %q, want its purpose and the agent", bar)
	}
	m.composing = true
	m.composeText = "fix the login bug"
	bar = ansi.Strip(m.renderComposer())
	if !strings.Contains(bar, "fix the login bug") {
		t.Fatalf("focused bar = %q, want the text being typed", bar)
	}
}

// The composer's cursor sits at the end of the text, so a task longer than the
// line keeps its end in view rather than its start.
func TestTailCellsKeepsTheEndBeingTypedAt(t *testing.T) {
	if got := tailCells("short", 20); got != "short" {
		t.Fatalf("tailCells(short) = %q, want it untouched", got)
	}
	got := tailCells("a very long task description", 10)
	if !strings.HasPrefix(got, "…") ||
		!strings.HasSuffix("a very long task description", strings.TrimPrefix(got, "…")) {
		t.Fatalf("tailCells(long) = %q, want the tail behind an ellipsis", got)
	}
	if lipgloss.Width(got) > 10 {
		t.Fatalf("tailCells(long) = %q is wider than its budget", got)
	}
}

// Clicking is resolved against the zones the last rendered frame recorded, so
// these tests render a frame the way the runtime would before every click.

func clickBoardModel() *Model {
	now := time.Now()
	m := &Model{
		adapter: &previewAdapter{},
		width:   120,
		height:  40,
		sessions: []agent.Session{
			{
				ID:            "newer",
				Agent:         "codex",
				Title:         "Newer session",
				CWD:           "/projects/alpha",
				RuntimeStatus: agent.StatusRunning,
				RecencyAt:     now,
				UpdatedAt:     now,
			},
			{
				ID:            "older",
				Agent:         "codex",
				Title:         "Older session",
				CWD:           "/projects/beta",
				RuntimeStatus: agent.StatusRunning,
				RecencyAt:     now,
				UpdatedAt:     now.Add(-time.Minute),
			},
		},
	}
	m.clampSelection()
	return m
}

func click(t *testing.T, m *Model, x, y int) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return cmd
}

// renderedZone draws a frame and returns the first zone the predicate
// accepts, which is how a test clicks what is actually on screen.
func renderedZone(t *testing.T, m *Model, accept func(clickZone) bool) clickZone {
	t.Helper()
	m.render()
	for _, zone := range m.clickZones {
		if accept(zone) {
			return zone
		}
	}
	t.Fatal("no rendered zone matched")
	return clickZone{}
}

func TestClickSelectsACardAndASecondClickPreviews(t *testing.T) {
	m := clickBoardModel()
	secondCard := func(z clickZone) bool {
		return z.action == clickCard && z.row == 1
	}

	zone := renderedZone(t, m, secondCard)
	if cmd := click(t, m, zone.rect.x, zone.rect.y); cmd != nil {
		t.Fatal("the selecting click already returned a command")
	}
	if m.column != zone.column || m.row != 1 {
		t.Fatalf("click selected %d/%d, want %d/1", m.column, m.row, zone.column)
	}
	if m.previewOpen {
		t.Fatal("the selecting click opened Quick Look")
	}

	zone = renderedZone(t, m, secondCard)
	if cmd := click(t, m, zone.rect.x, zone.rect.y); cmd == nil {
		t.Fatal("clicking the selected card returned no load command")
	}
	if !m.previewOpen {
		t.Fatal("clicking the selected card did not open Quick Look")
	}
}

func TestListRowsAreClickableToo(t *testing.T) {
	m := clickBoardModel()
	m.layout = layoutList

	zone := renderedZone(t, m, func(z clickZone) bool {
		return z.action == clickCard && z.row == 1
	})
	click(t, m, zone.rect.x, zone.rect.y)
	if m.column != zone.column || m.row != 1 {
		t.Fatalf("list click selected %d/%d, want %d/1", m.column, m.row, zone.column)
	}
}

func TestHeaderTabsSwitchGroupAndLayoutOnClick(t *testing.T) {
	m := clickBoardModel()

	zone := renderedZone(t, m, func(z clickZone) bool {
		return z.action == clickGroupProjects
	})
	click(t, m, zone.rect.x, zone.rect.y)
	if m.group != groupProject {
		t.Fatal("clicking the Projects tab did not switch the grouping")
	}

	zone = renderedZone(t, m, func(z clickZone) bool {
		return z.action == clickLayoutList
	})
	click(t, m, zone.rect.x, zone.rect.y)
	if m.layout != layoutList {
		t.Fatal("clicking the List tab did not switch the layout")
	}

	// Clicking the tab that is already lit changes nothing.
	m.column, m.row = 0, 1
	zone = renderedZone(t, m, func(z clickZone) bool {
		return z.action == clickGroupProjects
	})
	click(t, m, zone.rect.x, zone.rect.y)
	if m.group != groupProject || m.row != 1 {
		t.Fatal("clicking the active tab reset the selection")
	}
}

func TestClickOutsideQuickLookClosesIt(t *testing.T) {
	m := clickBoardModel()
	press(t, m, tea.KeyPressMsg{Code: ' ', Text: " "})
	settleQuickLook(m)
	m.render()

	inside := m.quickLookRect
	click(t, m, inside.x+inside.width/2, inside.y+inside.height/2)
	if !m.previewOpen {
		t.Fatal("a click inside the overlay closed it")
	}

	m.render()
	click(t, m, 0, 0)
	if m.previewOpen {
		t.Fatal("a click outside the overlay did not close it")
	}
}

func TestClickPutsDownTheComposerAndKeepsTheDraft(t *testing.T) {
	now := time.Now()
	m := composerModel(&fakeStarter{})
	m.sessions = []agent.Session{{
		ID:            "one",
		Agent:         "codex",
		Title:         "A session",
		CWD:           "/projects/alpha",
		RuntimeStatus: agent.StatusRunning,
		RecencyAt:     now,
		UpdatedAt:     now,
	}}
	m.clampSelection()

	zone := renderedZone(t, m, func(z clickZone) bool {
		return z.action == clickComposer
	})
	click(t, m, zone.rect.x+2, zone.rect.y+1)
	if !m.composing {
		t.Fatal("clicking the composer did not focus it")
	}
	typeText(t, m, "draft")

	card := renderedZone(t, m, func(z clickZone) bool {
		return z.action == clickCard
	})
	click(t, m, card.rect.x, card.rect.y)
	if m.composing {
		t.Fatal("clicking a card did not put the composer down")
	}
	if m.composeText != "draft" {
		t.Fatalf("composeText = %q, want the draft kept", m.composeText)
	}
}

func TestCompactTabsPageAndSwitchColumnsOnClick(t *testing.T) {
	now := time.Now()
	m := &Model{
		adapter: &previewAdapter{},
		width:   80,
		height:  30,
		sessions: []agent.Session{
			{ID: "r", RuntimeStatus: agent.StatusRunning, RecencyAt: now, UpdatedAt: now},
			{ID: "i", RuntimeStatus: agent.StatusIdle, RecencyAt: now, UpdatedAt: now},
		},
	}
	m.clampSelection()

	zone := renderedZone(t, m, func(z clickZone) bool {
		return z.action == clickColumnTab && z.column == 2
	})
	click(t, m, zone.rect.x, zone.rect.y)
	if m.column != 2 {
		t.Fatalf("clicking the Idle tab selected column %d, want 2", m.column)
	}
}

func TestWheelOnTheBoardMovesTheSelection(t *testing.T) {
	m := clickBoardModel()
	if m.row != 0 {
		t.Fatalf("selection starts at row %d, want 0", m.row)
	}

	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.row != 1 {
		t.Fatalf("wheel down moved to row %d, want 1", m.row)
	}
	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.row != 0 {
		t.Fatalf("wheel up moved to row %d, want 0", m.row)
	}

	m.detail = true
	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.row != 0 {
		t.Fatal("the wheel moved the selection behind the detail card")
	}
}

func TestCtrlKOpensSearchLikeSlash(t *testing.T) {
	m := clickBoardModel()
	press(t, m, tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	if !m.searching {
		t.Fatal("ctrl+k did not open the search input")
	}
}

func dismissModel(t *testing.T) *Model {
	t.Helper()
	store, err := dismiss.OpenAt(filepath.Join(t.TempDir(), "dismissed.json"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	now := time.Now()
	m := &Model{
		adapter:    &previewAdapter{},
		dismissals: store,
		width:      120,
		height:     40,
		group:      groupStatus,
		sessions: []agent.Session{
			{
				ID:            "one",
				Agent:         "codex",
				Title:         "First session",
				CWD:           "/projects/a",
				RuntimeStatus: agent.StatusIdle,
				UpdatedAt:     now,
				RecencyAt:     now,
			},
			{
				ID:            "two",
				Agent:         "codex",
				Title:         "Second session",
				CWD:           "/projects/b",
				RuntimeStatus: agent.StatusIdle,
				UpdatedAt:     now.Add(-time.Minute),
				RecencyAt:     now.Add(-time.Minute),
			},
		},
	}
	m.clampSelection()
	return m
}

func idleCards(m *Model) []agent.Session {
	for i, column := range m.columns() {
		if column.status == agent.StatusIdle {
			return m.cardsForColumn(i)
		}
	}
	return nil
}

var ctrlX = tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}

func TestDismissTakesTwoCtrlXPresses(t *testing.T) {
	m := dismissModel(t)

	press(t, m, ctrlX)
	if cards := idleCards(m); len(cards) != 2 {
		t.Fatalf("one ctrl+x already changed the board: %d cards", len(cards))
	}
	if !strings.Contains(m.status, "again") {
		t.Fatalf("arming did not ask for confirmation: %q", m.status)
	}

	press(t, m, ctrlX)
	cards := idleCards(m)
	if len(cards) != 1 || cards[0].ID != "two" {
		t.Fatalf("the confirmed dismissal did not remove the session: %v", cards)
	}
	if !m.dismissals.Dismissed("codex", "one") {
		t.Fatal("the dismissal was not recorded in the store")
	}
}

func TestAnotherKeyStandsDownAPendingDismissal(t *testing.T) {
	m := dismissModel(t)

	press(t, m, ctrlX)
	press(t, m, tea.KeyPressMsg{Code: 'v', Text: "v"})
	press(t, m, ctrlX)

	if m.dismissals.Dismissed("codex", "one") {
		t.Fatal("ctrl+x confirmed a dismissal another key had stood down")
	}
	if !strings.Contains(m.status, "again") {
		t.Fatalf("the third press did not re-arm: %q", m.status)
	}
}

func TestAnExpiredArmRearmsInsteadOfDismissing(t *testing.T) {
	m := dismissModel(t)

	press(t, m, ctrlX)
	m.pendingDismissAt = time.Now().Add(-2 * dismissConfirmWindow)
	press(t, m, ctrlX)

	if m.dismissals.Dismissed("codex", "one") {
		t.Fatal("a ctrl+x outside the window confirmed the dismissal")
	}
	if len(idleCards(m)) != 2 {
		t.Fatal("an expired arm still changed the board")
	}
}

func TestDismissedSessionsAreHiddenFromSearchToo(t *testing.T) {
	m := dismissModel(t)
	if err := m.dismissals.Dismiss("codex", "one"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	if m.sessionVisible(m.sessions[0], "first") {
		t.Fatal("a search surfaced a dismissed session")
	}
	if !m.sessionVisible(m.sessions[1], "second") {
		t.Fatal("hiding a dismissed session took an undismissed one with it")
	}
}

func TestCtrlXWithoutAStoreOnlyReportsIt(t *testing.T) {
	m := dismissModel(t)
	m.dismissals = nil

	press(t, m, ctrlX)
	press(t, m, ctrlX)

	if len(idleCards(m)) != 2 {
		t.Fatal("ctrl+x changed the board without a store to remember it")
	}
	if !strings.Contains(m.status, "unavailable") {
		t.Fatalf("the missing store was not reported: %q", m.status)
	}
}

func TestOnlyAnUnfilteredBoardPrunesDismissals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dismissed.json")
	stale := time.Now().Add(-2 * dismissRetention).UTC().Format(time.RFC3339)
	entry := []byte(`{"codex/gone": ` + strconv.Quote(stale) + `}`)
	if err := os.WriteFile(path, entry, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := dismiss.OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	m := dismissModel(t)
	m.dismissals = store

	// A filtered (-t) board only sees sessions in tmux panes, so a refresh
	// that no longer returns the session proves nothing about it.
	m.pruneDismissed = false
	m.Update(refreshMsg{sessions: m.sessions})
	if !store.Dismissed("codex", "gone") {
		t.Fatal("a filtered refresh pruned a dismissal it could not disprove")
	}

	m.pruneDismissed = true
	m.Update(refreshMsg{sessions: m.sessions})
	if store.Dismissed("codex", "gone") {
		t.Fatal("a full refresh kept a dismissal whose session is long gone")
	}
}

func TestFailedDismissLeavesTheSessionOnTheBoard(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := dismiss.OpenAt(filepath.Join(dir, "dismissed.json"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	m := dismissModel(t)
	m.dismissals = store

	press(t, m, ctrlX)
	press(t, m, ctrlX)

	if !strings.Contains(m.status, "failed") {
		t.Fatalf("the failed save was not reported: %q", m.status)
	}
	if len(idleCards(m)) != 2 {
		t.Fatal("a dismissal that was never saved still hid the session")
	}
}

func composerModelWithProjects(starter *fakeStarter) *Model {
	m := composerModel(starter)
	now := time.Now()
	m.sessions = []agent.Session{
		{
			ID:            "older",
			Agent:         "codex",
			CWD:           "/projects/alpha",
			RuntimeStatus: agent.StatusIdle,
			UpdatedAt:     now.Add(-time.Hour),
			RecencyAt:     now.Add(-time.Hour),
		},
		{
			ID:            "newer",
			Agent:         "claude",
			CWD:           "/projects/beta",
			RuntimeStatus: agent.StatusIdle,
			UpdatedAt:     now,
			RecencyAt:     now,
		},
	}
	return m
}

func TestComposerMentionPicksAProject(t *testing.T) {
	starter := &fakeStarter{}
	m := composerModelWithProjects(starter)

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "fix the bug @be")
	if got := m.composeMenuEntries(); len(got) != 1 || got[0] != "/projects/beta" {
		t.Fatalf("menu for @be offered %v, want just the matching project", got)
	}

	// Enter takes the pick rather than starting the session, and the token
	// leaves the text: the directory is the task's address, not its words.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.currentComposeDir() != "/projects/beta" {
		t.Fatalf("accepting the mention set the directory to %q", m.currentComposeDir())
	}
	if m.composeText != "fix the bug " {
		t.Fatalf("accepting the mention left the text as %q", m.composeText)
	}
	if starter.dir != "" {
		t.Fatal("enter on an open menu started the session")
	}

	typeText(t, m, "now")
	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if msg := cmd().(sessionStartedMsg); msg.err != nil {
		t.Fatalf("starting in a picked directory failed: %v", msg.err)
	}
	if starter.dir != "/projects/beta" {
		t.Fatalf("session started in %q, want the picked project", starter.dir)
	}
	if starter.command[len(starter.command)-1] != "fix the bug now" {
		t.Fatalf("session started with prompt %q", starter.command[len(starter.command)-1])
	}
}

func TestComposerMentionMenuNavigates(t *testing.T) {
	m := composerModelWithProjects(&fakeStarter{})

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "@")

	// A bare @ offers everywhere a task can start: the board's own
	// directory first, then projects freshest first.
	want := []string{"/projects/openagentview", "/projects/beta", "/projects/alpha"}
	got := m.composeMenuEntries()
	if len(got) != len(want) {
		t.Fatalf("bare @ offered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bare @ offered %v, want %v", got, want)
		}
	}

	press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.currentComposeDir() != "/projects/beta" {
		t.Fatalf("picking the second row set the directory to %q", m.currentComposeDir())
	}
}

func TestComposerMentionCompletesPaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := composerModelWithProjects(&fakeStarter{})

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "@"+root+"/al")

	// Tab completes shell style: the directory fills the token, open at its
	// end so the next segment can be typed straight away.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if want := "@" + filepath.Join(root, "alpha") + "/"; m.composeText != want {
		t.Fatalf("tab completed to %q, want %q", m.composeText, want)
	}

	// A fully typed directory is offered as itself, so enter can accept it
	// even though there is nothing below it to complete.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if want := filepath.Join(root, "alpha"); m.currentComposeDir() != want {
		t.Fatalf("accepting the path set the directory to %q, want %q", m.currentComposeDir(), want)
	}
	if m.composeText != "" {
		t.Fatalf("accepting the path left the text as %q", m.composeText)
	}
}

func TestComposerMentionEscKeepsTheText(t *testing.T) {
	starter := &fakeStarter{}
	m := composerModelWithProjects(starter)

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "@beta")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if !m.composing {
		t.Fatal("esc on an open menu put the whole composer down")
	}
	if len(m.composeMenuEntries()) != 0 {
		t.Fatal("esc left the menu up")
	}

	// With the menu stood down the @ is literal text, and enter means what
	// it always means: start the session.
	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if msg := cmd().(sessionStartedMsg); msg.err != nil {
		t.Fatalf("starting with a literal @ failed: %v", msg.err)
	}
	if starter.command[len(starter.command)-1] != "@beta" {
		t.Fatalf("session started with prompt %q, want the literal text", starter.command[len(starter.command)-1])
	}
	if starter.dir != "/projects/openagentview" {
		t.Fatalf("session started in %q, want the board's directory", starter.dir)
	}
}

func TestComposerMentionWithNoMatchHoldsEnter(t *testing.T) {
	starter := &fakeStarter{}
	m := composerModelWithProjects(starter)

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "task @zzz")
	if got := m.composeMenuEntries(); len(got) != 0 {
		t.Fatalf("a query matching nothing offered %v", got)
	}

	// The token still owns enter: starting now would ship the typo as prompt
	// text and the task to the wrong directory.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if starter.dir != "" {
		t.Fatal("enter on a matchless mention started the session")
	}
	if !m.composing || m.composeText != "task @zzz" {
		t.Fatalf("holding enter disturbed the draft: composing=%v text=%q", m.composing, m.composeText)
	}

	// Esc is the deliberate way to keep the literal @, after which enter
	// means what it always means.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if msg := cmd().(sessionStartedMsg); msg.err != nil {
		t.Fatalf("starting after esc failed: %v", msg.err)
	}
	if starter.command[len(starter.command)-1] != "task @zzz" {
		t.Fatalf("session started with prompt %q, want the literal text", starter.command[len(starter.command)-1])
	}
}

func TestComposerMentionCompletesSpacedPaths(t *testing.T) {
	root := t.TempDir()
	spaced := filepath.Join(root, "My Project")
	if err := os.Mkdir(spaced, 0o755); err != nil {
		t.Fatal(err)
	}
	m := composerModelWithProjects(&fakeStarter{})

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "@"+root+"/My")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})

	if want := "@" + strings.ReplaceAll(spaced, " ", `\ `) + "/"; m.composeText != want {
		t.Fatalf("tab completed to %q, want the space escaped as %q", m.composeText, want)
	}
	// The escaped space keeps the token in one piece, so the completed path
	// is still the menu's answer and enter can take it.
	if got := m.composeMenuEntries(); len(got) != 1 || got[0] != spaced {
		t.Fatalf("menu after completing a spaced path offered %v", got)
	}
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.currentComposeDir() != spaced {
		t.Fatalf("accepting the spaced path set the directory to %q", m.currentComposeDir())
	}
}

func TestComposerMenuFitsAShortTerminal(t *testing.T) {
	m := composerModelWithProjects(&fakeStarter{})
	m.height = 13

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "@")

	// 13 rows leave exactly one for the menu once the header, the board's
	// own floor, the input line and the footer have taken theirs.
	if rows := m.composeMenuRows(); len(rows) != 1 {
		t.Fatalf("a 13-row terminal got %d menu rows", len(rows))
	}
	if got, room := m.composerHeight(), m.height-4-m.footerHeight()-minBoardHeight; got > room {
		t.Fatalf("composer takes %d rows, more than the %d the terminal has spare", got, room)
	}
}

func TestComposerMenuCapHoldsForTrailingSlashPaths(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := composerModelWithProjects(&fakeStarter{})
	m.height = 13

	// A trailing slash puts the directory itself in the menu before its
	// children are listed; the single-row budget must hold from there too,
	// and tab completing writes exactly this shape of query.
	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "@"+root+"/")

	if got := m.composeMenuEntries(); len(got) != 1 {
		t.Fatalf("a 13-row terminal got %d entries for a trailing-slash path", len(got))
	}
	if rows := m.composeMenuRows(); len(rows) != 1 {
		t.Fatalf("a 13-row terminal got %d menu rows for a trailing-slash path", len(rows))
	}
	if got, room := m.composerHeight(), m.height-4-m.footerHeight()-minBoardHeight; got > room {
		t.Fatalf("composer takes %d rows, more than the %d the terminal has spare", got, room)
	}
}

func TestComposerAtInsideAWordIsNotAMention(t *testing.T) {
	m := composerModelWithProjects(&fakeStarter{})

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "email a@beta")

	if got := m.composeMenuEntries(); len(got) != 0 {
		t.Fatalf("an @ inside a word opened the menu: %v", got)
	}
}

func TestComposerDirSurvivesTheDraftBeingPutDown(t *testing.T) {
	starter := &fakeStarter{}
	m := composerModel(starter)
	m.sessions = []agent.Session{{
		ID:            "one",
		Agent:         "codex",
		CWD:           "/projects/alpha",
		RuntimeStatus: agent.StatusIdle,
		UpdatedAt:     time.Now(),
		RecencyAt:     time.Now(),
	}}

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	typeText(t, m, "@alpha")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})

	if m.currentComposeDir() != "/projects/alpha" {
		t.Fatalf("reopening the composer forgot the picked directory: %q", m.currentComposeDir())
	}
}
