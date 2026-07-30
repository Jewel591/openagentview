package ui

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Jewel591/openagentview/internal/agent"
	"github.com/Jewel591/openagentview/internal/dismiss"
	"github.com/Jewel591/openagentview/internal/prefs"
	"github.com/Jewel591/openagentview/internal/terminal"
	"github.com/Jewel591/openagentview/internal/tmux"
)

// The board is set along two independent axes: what the sessions are grouped
// by, and how the groups are drawn. Any grouping can be read in any layout.
type boardGroup int

const (
	groupStatus boardGroup = iota
	groupProject
)

const boardGroupCount = 2

type boardLayout int

const (
	layoutKanban boardLayout = iota
	layoutList
)

const recentWindow = 24 * time.Hour

// A dismissal is asked for twice: one ctrl+x arms it, a second ctrl+x on the
// same session inside this window confirms it, and any other keystroke stands
// it down. Two presses because there is no agent-side record to undo.
const dismissConfirmWindow = 2 * time.Second

// A dismissed session that discovery stops returning keeps its entry this
// long before it is pruned, so the file stays bounded by the sessions that
// exist without a briefly hidden session losing its dismissal.
const dismissRetention = 30 * 24 * time.Hour

const previewRefreshInterval = time.Second

// Transcript Quick Look starts with enough messages for an ordinary window and
// only asks for more when the reader reaches the top. Each explicit expansion
// makes one bounded tail rebuild; the one-second poll keeps using the same
// limit, so repeated reads still cost only what the agent appended.
const (
	previewMessagePage     = 16
	previewMessageLimitMax = 4096
)

// A mirrored pane is a screen rather than a transcript, so it is polled faster
// than a rollout log: the point of watching one is seeing the agent move.
// Typing tightens it further, since a keystroke that takes half a second to
// appear does not feel like typing at all.
const (
	paneRefreshInterval      = 400 * time.Millisecond
	paneInputRefreshInterval = 120 * time.Millisecond
)

// paneEscapeKey leaves typing and hands the board's keys back. Esc is the
// one key the mirror keeps for itself — agents bind it too, but modal editing
// has meant "esc leaves the mode" for too long to spell it any other way, and
// interrupting the agent stays a ctrl+c away. ctrl+] is kept as a quiet alias
// for the hands already used to it.
const paneEscapeKey = "ctrl+]"

// Quick Look zooms out of the selected card the way the macOS original zooms
// out of a file icon: briefly, easing out, and from the thing that was asked
// about. Long enough to read as motion, short enough to never be waited on.
const (
	previewOpenDuration      = 160 * time.Millisecond
	previewOpenFrameInterval = time.Second / 60
)

// screenRect is a rectangle in terminal cells, used to remember where the
// selected card sits and to grow the overlay out of it.
type screenRect struct {
	x, y, width, height int
}

func (r screenRect) contains(x, y int) bool {
	return x >= r.x && x < r.x+r.width && y >= r.y && y < r.y+r.height
}

// A clickZone is one rectangle of the last rendered frame and the action a
// click inside it asks for. Zones are recorded while rendering, where every
// widget's position is already known, rather than re-derived from the layout
// arithmetic in a handler that would have to be kept in step with it.
type clickAction int

const (
	clickGroupStatus clickAction = iota
	clickGroupProjects
	clickLayoutKanban
	clickLayoutList
	clickSearch
	clickAgentChip
	clickComposer
	clickCard
	clickColumnTab
	clickColumnPrev
	clickColumnNext
)

type clickZone struct {
	rect   screenRect
	action clickAction
	// column and row name the card a clickCard zone stands for, and column
	// alone the tab a clickColumnTab zone stands for or the place in the
	// header run a clickAgentChip zone stands for.
	column int
	row    int
}

const refreshFailedPrefix = "Refresh failed: "

type refreshMsg struct {
	sessions []agent.Session
	err      error
}

type previewLoadedMsg struct {
	sessionID  string
	generation uint64
	limit      int
	transcript agent.Transcript
	err        error
}

type previewRefreshMsg struct {
	generation uint64
}

type paneLoadedMsg struct {
	sessionID  string
	generation uint64
	screen     tmux.Screen
	err        error
	// poll marks the captures that belong to the polling chain. Captures fired
	// off after a keystroke must not schedule their own next tick, or every
	// keystroke would leave a second poller running behind it.
	poll bool
}

type paneRefreshMsg struct {
	generation uint64
}

// previewAnimMsg is one frame of the overlay's opening zoom.
type previewAnimMsg struct {
	generation uint64
}

// paneSentMsg reports a keystroke that was forwarded to a pane. It exists to
// re-capture immediately rather than waiting out the poll, so typing echoes at
// the speed of the send instead of the refresh interval.
type paneSentMsg struct {
	generation uint64
	err        error
}

// sessionStartedMsg reports the composer's attempt to start a fresh agent in a
// tmux session of its own. The prompt rides along so a failed attempt can hand
// the draft back instead of losing it with the session that never started.
type sessionStartedMsg struct {
	agent  string
	name   string
	prompt string
	err    error
}

type resumeFinishedMsg struct {
	err error
}

// tabOpenedMsg is the terminal's answer to a new-tab request.
type tabOpenedMsg struct {
	err error
}

type tickMsg time.Time

type previewBackdrop struct {
	baseLines []string
	left      []string
	right     []string
	x         int
	y         int
	boxWidth  int
	boxHeight int
	baseBytes int
}

type Model struct {
	adapter   agent.Adapter
	panes     PaneController
	starter   SessionStarter
	launchers []Launcher
	// opener asks the surrounding terminal for a new tab, nil when the
	// terminal is not one the board knows how to ask — going to a session
	// then falls back to handing this window over.
	opener TabOpener
	// dismissals is the board's own record of sessions asked off it, nil when
	// its state file could not be read — the board still works, and ctrl+x
	// says what is wrong instead of writing over the file.
	dismissals *dismiss.Store
	// pruneDismissed is false when discovery is filtered (-t): a board that
	// only sees sessions in tmux panes cannot prove any other session gone,
	// and pruning on such evidence would put dismissed sessions back.
	pruneDismissed bool
	// pendingDismissID is the session one ctrl+x has armed for dismissal, and
	// pendingDismissAt when, so the confirming press can be held to the same
	// session and a short window.
	pendingDismissID string
	pendingDismissAt time.Time
	// workdir is where a composed session starts: the directory openagentview
	// itself was launched from, resolved once rather than per task.
	workdir string

	composing    bool
	composeText  string
	composeAgent int
	// composeDir is where the composed task will start, empty meaning the
	// board's own working directory. It survives the composer being put down,
	// the way the agent choice does: the next task usually goes where the
	// last one went.
	composeDir string
	// composeMenuSel is the highlighted row of the @ completion menu, reset
	// whenever the text changes since the entries under it change too.
	composeMenuSel int
	// composeMenuHidden is set when esc puts the menu away for the token
	// being typed — the @ was literal text after all. Any edit brings the
	// menu back, since new text is a new question.
	composeMenuHidden bool

	sessions  []agent.Session
	group     boardGroup
	layout    boardLayout
	column    int
	row       int
	width     int
	height    int
	query     string
	searching bool
	// agentFilter is the set of agents the board is narrowed to, empty
	// meaning every agent. Each header chip toggles its own entry, so the
	// filter reads as "these agents" rather than a single choice cycled
	// through — two agents at once is a question the board is asked often.
	agentFilter map[string]bool
	// preferences remembers the grouping, layout and agent filter between
	// runs. Nil means the state file could not be opened: the board still
	// works, it just starts from its defaults next time.
	preferences         *prefs.Store
	detail              bool
	helpOpen            bool
	previewOpen         bool
	previewLoading      bool
	previewSessionID    string
	previewSession      agent.Session
	previewGeneration   uint64
	previewMessages     []agent.TranscriptMessage
	previewStatus       agent.RuntimeStatus
	previewActivity     agent.Activity
	previewErr          error
	previewScrollBack   int
	previewMessageLimit int
	previewLoadedLimit  int
	// previewTranscriptExhausted records that the adapter returned fewer
	// messages than requested (or no growth for a larger request), so repeated
	// wheel inertia at the oldest message cannot keep rebuilding a large log.
	previewTranscriptExhausted   bool
	previewTranscriptLoadingMore bool
	previewPendingScrollBack     int
	// previewTranscriptReturnsToPane distinguishes an automatic continuation
	// from an explicit t-toggle. Only the automatic route treats the bottom of
	// the transcript as the doorway back to the live pane.
	previewTranscriptReturnsToPane bool
	previewReturnPaneInput         bool
	previewLayout                  []string
	previewLayoutWidth             int
	previewWrapped                 map[string][]string
	previewWrapWidth               int
	previewBase                    string
	previewBackdrop                previewBackdrop
	paneView                       bool
	paneInput                      bool
	paneLines                      []string
	// paneLoaded distinguishes a real zero-history answer from the zero value
	// before the opening capture lands. paneHistoryRequested is the scrollback
	// depth of the capture in flight or most recently accepted, so trackpad
	// inertia cannot launch the same expansion once per wheel event.
	paneLoaded           bool
	paneHistoryRequested int
	// paneAnchor is the length of the pane's content — scrollback held plus
	// screen captured — at the last load, which is what a scrolled-back mirror
	// measures its keep-your-place adjustment against.
	paneAnchor int
	paneWidth  int
	paneErr    error
	paneCursor tmux.Screen
	// paneScreenWidth is the pane's own width, which is what decides whether the
	// mirror can be a window with the board still visible around it.
	paneScreenWidth int
	// paneCursorRow and paneCursorCol are where the pane's cursor lands on this
	// terminal's screen, the row being -1 when it is not on show. They are worked
	// out while rendering, since that is where the mirror's scroll position and
	// the window's origin are both known.
	paneCursorRow int
	paneCursorCol int
	// clickZones is where everything clickable sat the last time the board was
	// drawn, so a click is resolved against the frame that was actually on
	// screen when it happened.
	clickZones []clickZone
	// quickLookRect and detailRect are where those overlays sat when last
	// drawn; a click outside either one is a request to put it down.
	quickLookRect screenRect
	detailRect    screenRect
	// selectedRect is where the selected card sat the last time the board was
	// drawn, which is the spot Quick Look grows out of.
	selectedRect    screenRect
	previewAnimFrom screenRect
	previewOpenedAt time.Time
	// previewAnimGeneration is deliberately separate from previewGeneration:
	// the content generation is bumped whenever a different load starts —
	// toggling the transcript, restarting the poll — and the zoom must ride
	// out those bumps rather than freeze on its first frame.
	previewAnimGeneration uint64
	previewAnimating      bool
	loading               bool
	status                string
	lastSync              time.Time
	// animFrame drives the running marker's pulse; animating is whether the
	// frame timer is armed, so refreshes do not stack a second one.
	animFrame int
	animating bool
}

// TabOpener opens a command in a new tab of the surrounding terminal.
type TabOpener interface {
	OpenTab(dir string, command []string) error
}

// PaneController is the part of tmux Quick Look needs: a live mirror of a
// pane, and a way to answer whatever the agent in it is waiting on. A
// positive history asks the capture for that much scrollback above the
// screen; zero keeps it to the screen the poll actually needs.
type PaneController interface {
	Capture(ctx context.Context, paneID string, history int) (tmux.Screen, error)
	SendText(ctx context.Context, paneID, text string) error
	SendKey(ctx context.Context, paneID, key string) error
}

// SessionStarter is the part of tmux the composer needs: somewhere for a fresh
// agent to run that the board can then discover, mirror and type into. Width
// and height size the detached session's pane; zero leaves the size to tmux.
type SessionStarter interface {
	NewSession(
		ctx context.Context,
		name, dir string,
		command []string,
		width, height int,
	) (string, error)
}

// Launcher is one agent the composer can hand a task to: the name it carries
// on the board, and the command that opens a fresh interactive session around
// an opening prompt.
type Launcher struct {
	Agent   string
	Command func(prompt string) (string, []string)
}

func New(
	adapter agent.Adapter,
	panes PaneController,
	starter SessionStarter,
	launchers []Launcher,
	dismissals *dismiss.Store,
	preferences *prefs.Store,
	pruneDismissed bool,
) *Model {
	workdir, err := os.Getwd()
	if err != nil {
		workdir = ""
	}
	// A typed nil must not become a non-nil interface, or the fallback
	// below it would never run.
	var opener TabOpener
	if detected := terminal.Detect(); detected != nil {
		opener = detected
	}
	m := &Model{
		adapter:        adapter,
		opener:         opener,
		panes:          panes,
		starter:        starter,
		launchers:      launchers,
		dismissals:     dismissals,
		preferences:    preferences,
		pruneDismissed: pruneDismissed,
		workdir:        workdir,
		loading:        true,
		width:          120,
		height:         36,
	}
	m.applyPrefs()
	return m
}

// applyPrefs starts the board the way it was left. Anything the file says
// that the board cannot mean — a misspelled grouping, an agent that no
// longer exists — falls back to the default silently: preferences are a
// convenience, never an error.
func (m *Model) applyPrefs() {
	if m.preferences == nil {
		return
	}
	saved := m.preferences.Load()
	if saved.Group == "projects" {
		m.group = groupProject
	}
	if saved.Layout == "list" {
		m.layout = layoutList
	}
	if len(saved.Agents) > 0 {
		m.agentFilter = map[string]bool{}
		for _, name := range saved.Agents {
			m.agentFilter[strings.ToLower(name)] = true
		}
	}
}

// savePrefs writes the board's arrangement as it is now. A failed write is
// dropped rather than surfaced — the board in front of the user is already
// right, and the only thing lost is the next start's head start.
func (m *Model) savePrefs() {
	if m.preferences == nil {
		return
	}
	saved := prefs.Prefs{Group: "status", Layout: "kanban"}
	if m.group == groupProject {
		saved.Group = "projects"
	}
	if m.layout == layoutList {
		saved.Layout = "list"
	}
	for name := range m.agentFilter {
		saved.Agents = append(saved.Agents, name)
	}
	sort.Strings(saved.Agents)
	_ = m.preferences.Save(saved)
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *Model) refreshCmd() tea.Cmd {
	adapter := m.adapter
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Sessions and an error can arrive together when only some of the
		// configured agents are reachable.
		sessions, err := adapter.Discover(ctx, 5000)
		return refreshMsg{sessions: sessions, err: err}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		widthChanged := m.width != msg.Width
		heightChanged := m.height != msg.Height
		m.width = msg.Width
		m.height = msg.Height
		if widthChanged && m.previewOpen {
			m.rebuildPreviewLayout()
		}
		if (widthChanged || heightChanged) && m.previewOpen {
			m.clampPreviewScrollBack()
		}
		if m.previewOpen {
			m.setPreviewBase(m.renderBase())
		}
		return m, nil
	case refreshMsg:
		// Keep Quick Look's interaction path isolated from the comparatively
		// expensive session discovery and selection restoration work.
		if m.previewOpen {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.status = refreshFailedPrefix + msg.err.Error()
			// One agent failing still leaves the others' sessions worth showing.
			if msg.sessions == nil {
				return m, nil
			}
		} else if strings.HasPrefix(m.status, refreshFailedPrefix) {
			m.status = ""
		}
		selectedID := ""
		if selected := m.selected(); selected != nil {
			selectedID = selected.ID
		}
		m.sessions = msg.sessions
		m.lastSync = time.Now()
		m.restoreSelection(selectedID)
		// Dismissals of sessions that no longer exist are pruned here, against
		// a full answer only: a partial one is missing a whole agent's
		// sessions, not evidence that they are gone.
		if msg.err == nil && m.dismissals != nil && m.pruneDismissed {
			present := make(map[string]bool, len(msg.sessions))
			for _, session := range msg.sessions {
				present[dismiss.Key(session.Agent, session.ID)] = true
			}
			cutoff := time.Now().Add(-dismissRetention)
			if err := m.dismissals.Prune(present, cutoff); err != nil {
				m.status = "Dismissal cleanup failed: " + err.Error()
			}
		}
		return m, m.startAnimIfNeeded()
	case previewLoadedMsg:
		if !m.previewOpen ||
			msg.sessionID != m.previewSessionID ||
			msg.generation != m.previewGeneration {
			return m, nil
		}
		wasLoading := m.previewLoading
		m.previewLoading = false
		m.previewTranscriptLoadingMore = false
		m.previewStatus = msg.transcript.Status
		m.previewActivity = msg.transcript.Activity
		oldMessageCount := len(m.previewMessages)
		oldLoadedLimit := m.previewLoadedLimit
		requestedOlder := oldLoadedLimit > 0 && msg.limit > oldLoadedLimit
		if wasLoading ||
			previewChanged(
				m.previewMessages,
				m.previewErr,
				msg.transcript.Messages,
				msg.err,
			) {
			oldLineCount := len(m.previewLayout)
			m.previewMessages = msg.transcript.Messages
			m.previewErr = msg.err
			m.rebuildPreviewLayout()
			// Normal polling adds content at the live bottom, so increasing the
			// bottom-relative offset by the layout growth holds a reader's
			// viewport still. A larger message limit prepends older content:
			// leaving the offset alone holds the old first row in place, then
			// the pending boundary gesture moves just a few rows above it.
			if m.previewScrollBack > 0 && !requestedOlder {
				m.previewScrollBack += len(m.previewLayout) - oldLineCount
			}
			m.clampPreviewScrollBack()
		}
		if msg.err == nil {
			m.previewLoadedLimit = max(m.previewLoadedLimit, msg.limit)
			m.previewTranscriptExhausted =
				len(msg.transcript.Messages) < msg.limit ||
					(requestedOlder && len(msg.transcript.Messages) <= oldMessageCount) ||
					msg.limit >= previewMessageLimitMax
			if m.previewPendingScrollBack > 0 {
				m.setPreviewScrollBack(
					m.previewScrollBack + m.previewPendingScrollBack,
				)
				m.previewPendingScrollBack = 0
			}
		}
		// Poll for as long as the overlay is open. Liveness is deliberately not
		// consulted here: session discovery is paused behind the overlay, so any
		// status we could test would be frozen at the moment it opened, and a
		// session that starts working while being watched would never refresh.
		generation := m.previewGeneration
		return m, tea.Tick(previewRefreshInterval, func(time.Time) tea.Msg {
			return previewRefreshMsg{generation: generation}
		})
	case previewRefreshMsg:
		if !m.previewOpen || msg.generation != m.previewGeneration {
			return m, nil
		}
		session := m.previewedSession()
		if session == nil {
			return m, nil
		}
		return m, m.loadPreview(*session, msg.generation)
	case paneLoadedMsg:
		if !m.previewOpen ||
			!m.paneView ||
			msg.sessionID != m.previewSessionID ||
			msg.generation != m.previewGeneration {
			return m, nil
		}
		m.previewLoading = false
		m.paneErr = msg.err
		if msg.err == nil {
			m.paneLoaded = true
			m.paneHistoryRequested = msg.screen.History
			m.paneCursor = msg.screen
			// Rows are kept down to the cursor even when they are empty: the
			// cursor sits below the last written line whenever an agent is
			// waiting on an empty composer, which is exactly when someone is
			// looking for it. The cursor is a screen fact, so any captured
			// scrollback sits above it.
			keep := len(msg.screen.Lines)
			if msg.screen.CursorVisible {
				keep = msg.screen.History + msg.screen.CursorY + 1
			}
			m.paneLines = trimBlankTail(msg.screen.Lines, keep)
			// Only the screen portion measures the pane: scrollback can hold
			// lines from a former, wider life of the pane, and sizing the
			// window to those would snap it wider mid-scroll.
			screenPortion := m.paneLines
			if h := msg.screen.History; h > 0 && h < len(screenPortion) {
				screenPortion = screenPortion[h:]
			}
			m.paneWidth = widestLine(screenPortion)
			// A tmux too old to report the pane's size leaves the widest line as
			// the only measure of it available.
			m.paneScreenWidth = max(msg.screen.Width, m.paneWidth)
			// The pane keeps printing while someone reads backwards, and every
			// printed line pushes the content they are reading one row further
			// from the bottom the scroll is anchored to. Growing the offset by
			// what the content grew holds their place; captured scrollback is
			// excluded, since rows above change no distance from the bottom.
			anchor := msg.screen.HistorySize +
				len(msg.screen.Lines) - msg.screen.History
			if m.previewScrollBack > 0 && m.paneAnchor > 0 {
				m.previewScrollBack += anchor - m.paneAnchor
			}
			m.paneAnchor = anchor
			m.clampPreviewScrollBack()
		} else {
			// Let the next deliberate scroll retry a failed history expansion.
			m.paneHistoryRequested = m.paneCursor.History
		}
		if !msg.poll || m.previewScrollBack > 0 {
			return m, nil
		}
		generation := m.previewGeneration
		return m, tea.Tick(m.paneInterval(), func(time.Time) tea.Msg {
			return paneRefreshMsg{generation: generation}
		})
	case paneRefreshMsg:
		if !m.previewOpen || !m.paneView || msg.generation != m.previewGeneration {
			return m, nil
		}
		session := m.previewedSession()
		if session == nil {
			return m, nil
		}
		return m, m.loadPane(*session, msg.generation, true)
	case paneSentMsg:
		if !m.previewOpen || !m.paneView || msg.generation != m.previewGeneration {
			return m, nil
		}
		if msg.err != nil {
			m.paneErr = msg.err
			return m, nil
		}
		// A scrolled mirror is deliberately frozen like terminal copy mode.
		// The keystroke still reached the agent; its echo appears when the
		// reader returns to the live bottom.
		if m.previewScrollBack > 0 {
			return m, nil
		}
		session := m.previewedSession()
		if session == nil {
			return m, nil
		}
		return m, m.loadPane(*session, msg.generation, false)
	case tickMsg:
		if m.previewOpen {
			return m, tickCmd()
		}
		return m, tea.Batch(m.refreshCmd(), tickCmd())
	case sessionStartedMsg:
		if msg.err != nil {
			m.status = "Start failed: " + msg.err.Error()
			// The draft comes back rather than dying with the attempt — unless
			// a newer draft is already being written over it.
			if m.composeText == "" {
				m.composeText = msg.prompt
			}
			return m, nil
		}
		m.status = "Started " + msg.agent + " in tmux session " + msg.name
		return m, m.refreshCmd()
	case resumeFinishedMsg:
		if msg.err != nil {
			m.status = "Resume failed: " + msg.err.Error()
		}
		return m, m.refreshCmd()
	case tabOpenedMsg:
		if msg.err != nil {
			m.status = "Opening a tab failed: " + msg.err.Error()
		} else {
			m.status = "Opened in a new tab"
		}
		return m, m.refreshCmd()
	case previewAnimMsg:
		if !m.previewOpen ||
			!m.previewAnimating ||
			msg.generation != m.previewAnimGeneration {
			return m, nil
		}
		if time.Since(m.previewOpenedAt) >= previewOpenDuration {
			m.previewAnimating = false
			return m, nil
		}
		return m, m.previewAnimTick()
	case tea.MouseWheelMsg:
		return m.handleWheel(msg)
	case tea.MouseClickMsg:
		return m.handleClick(msg)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		return m.handlePaste(msg.Content)
	case animTickMsg:
		// The pulse runs only while the list is showing something that is
		// actually working — kanban never draws the marker, so animating it
		// there would be redrawing the board for nothing — and rests while
		// Quick Look owns the screen. A refresh or a switch back to the
		// list starts it up again.
		if m.previewOpen || m.layout != layoutList || !m.anyRunningVisible() {
			m.animating = false
			return m, nil
		}
		m.animFrame++
		return m, animTickCmd()
	}
	return m, nil
}

type animTickMsg time.Time

// animFrameInterval paces the running marker's pulse: fast enough to read
// as motion, slow enough that a board full of text is not redrawn for sport.
const animFrameInterval = 300 * time.Millisecond

func animTickCmd() tea.Cmd {
	return tea.Tick(animFrameInterval, func(t time.Time) tea.Msg {
		return animTickMsg(t)
	})
}

// startAnimIfNeeded arms the pulse — after a refresh, or after the layout
// switches to the list — when the list is up, something is running, and the
// pulse is not already going; the animating flag is what keeps two callers
// from stacking two timers.
func (m *Model) startAnimIfNeeded() tea.Cmd {
	if m.animating || m.previewOpen || m.layout != layoutList ||
		!m.anyRunningVisible() {
		return nil
	}
	m.animating = true
	return animTickCmd()
}

func (m *Model) anyRunningVisible() bool {
	query := strings.ToLower(strings.TrimSpace(m.query))
	for _, session := range m.sessions {
		if session.RuntimeStatus == agent.StatusRunning &&
			m.sessionVisible(session, query) {
			return true
		}
	}
	return false
}

// handlePaste routes bracketed-paste text to whichever input holds the
// keyboard, in the same precedence handleKey gives them. Terminals send
// cmd+v as a paste event rather than keystrokes, so before this the board
// silently dropped it — a prepared task description is exactly the kind of
// text that arrives by paste.
func (m *Model) handlePaste(text string) (tea.Model, tea.Cmd) {
	if m.searching {
		m.query += singleLine(text)
		return m, nil
	}
	if m.composing {
		m.composeText += singleLine(text)
		m.composeTextEdited()
		return m, nil
	}
	if m.previewOpen && m.paneInput {
		// The mirrored agent gets the paste untouched: what its TUI does
		// with newlines is its own contract.
		return m, m.sendPaneText(text)
	}
	return m, nil
}

// singleLine folds pasted text into the one line the board's inputs hold:
// line breaks and tabs become spaces, so a copied paragraph lands as a
// prompt instead of leaving stray control characters in the input.
func singleLine(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		}
		return r
	}, text)
}

// wheelScrollLines is how far one wheel notch moves the overlay. Three lines
// per notch is what scrolling feels like everywhere else on the desktop.
const wheelScrollLines = 3

// handleWheel scrolls the Quick Look body, the one thing on screen that
// actually scrolls. On the board it does nothing: the board is one screen,
// and a trackpad's inertia fires dozens of wheel events per flick — mapped
// to the selection, a stray swipe sent it flying, so keys and clicks select.
// In Quick Look the wheel never reaches the mirrored agent: pointing a wheel
// at a transcript is always a request to read it, even mid-typing, whereas
// the arrow keys it would otherwise become walk an agent's input history.
func (m *Model) handleWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	if !m.previewOpen {
		return m, nil
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		return m, m.scrollPreviewBy(wheelScrollLines)
	case tea.MouseWheelDown:
		return m, m.scrollPreviewBy(-wheelScrollLines)
	}
	return m, nil
}

// scrollPreviewBy carries one reading gesture across the two honest history
// sources. tmux owns the current rendered pane and whatever normal-screen
// scrollback it retained; the agent transcript owns the older conversation.
// Crossing either boundary is explicit in the UI, but does not make the user
// stop scrolling and hunt for a mode key.
func (m *Model) scrollPreviewBy(delta int) tea.Cmd {
	if delta == 0 {
		return nil
	}
	before := m.previewScrollBack
	desired := before + delta
	maxScroll := m.previewMaxScrollBack()
	if delta > 0 && desired > maxScroll {
		overflow := max(1, desired-maxScroll)
		switch {
		case m.paneView && m.paneLoaded:
			return m.continuePaneIntoTranscript(overflow)
		case !m.paneView && m.previewLoading:
			m.previewPendingScrollBack = max(
				m.previewPendingScrollBack,
				overflow,
			)
			return nil
		case !m.paneView && m.previewTranscriptLoadingMore:
			m.previewPendingScrollBack = max(
				m.previewPendingScrollBack,
				overflow,
			)
			return nil
		case !m.paneView && !m.previewTranscriptExhausted:
			if cmd := m.loadMorePreviewMessages(overflow); cmd != nil {
				return cmd
			}
		}
	}
	if delta < 0 &&
		!m.paneView &&
		m.previewTranscriptReturnsToPane &&
		desired <= 0 {
		return m.returnToLivePane()
	}
	m.setPreviewScrollBack(desired)
	return m.paneScrollRefresh(before)
}

func (m *Model) continuePaneIntoTranscript(overflow int) tea.Cmd {
	session := m.previewedSession()
	if session == nil {
		return nil
	}
	m.previewTranscriptReturnsToPane = true
	m.previewReturnPaneInput = m.paneInput
	m.paneView = false
	m.paneInput = false
	m.previewLoading = true
	m.previewScrollBack = 0
	m.previewMessageLimit = previewMessagePage
	m.previewLoadedLimit = 0
	m.previewTranscriptExhausted = false
	m.previewTranscriptLoadingMore = false
	m.previewPendingScrollBack = max(1, overflow)
	m.previewMessages = nil
	m.previewErr = nil
	m.previewGeneration++
	m.rebuildPreviewLayout()
	return m.loadPreview(*session, m.previewGeneration)
}

func (m *Model) returnToLivePane() tea.Cmd {
	session := m.previewedSession()
	if session == nil || !m.canMirrorPane(*session) {
		return nil
	}
	m.paneView = true
	m.paneInput = m.previewReturnPaneInput
	m.previewTranscriptReturnsToPane = false
	m.previewReturnPaneInput = false
	m.previewPendingScrollBack = 0
	m.previewTranscriptLoadingMore = false
	m.previewLoading = true
	m.previewScrollBack = 0
	m.paneLoaded = false
	m.paneHistoryRequested = 0
	m.paneAnchor = 0
	m.previewGeneration++
	return m.loadPane(*session, m.previewGeneration, true)
}

func (m *Model) loadMorePreviewMessages(overflow int) tea.Cmd {
	session := m.previewedSession()
	if session == nil ||
		m.previewLoadedLimit <= 0 ||
		m.previewMessageLimit >= previewMessageLimitMax {
		m.previewTranscriptExhausted = true
		return nil
	}
	next := min(
		previewMessageLimitMax,
		max(previewMessagePage, m.previewMessageLimit*2),
	)
	if next <= m.previewMessageLimit {
		m.previewTranscriptExhausted = true
		return nil
	}
	m.previewMessageLimit = next
	m.previewTranscriptLoadingMore = true
	m.previewPendingScrollBack = max(
		m.previewPendingScrollBack,
		max(1, overflow),
	)
	// Retire the current poll. The expanded answer starts exactly one new
	// polling chain, while the existing laid-out transcript remains visible.
	m.previewGeneration++
	return m.loadPreview(*session, m.previewGeneration)
}

// previewMaxScrollBack is the oldest row the active Quick Look data source can
// honestly show right now. A transcript can ask its adapter for an older page
// when this loaded boundary is crossed. A pane has the current captured screen
// plus however much history tmux says it retains, which may be zero for a
// full-screen TUI using the alternate screen.
func (m *Model) previewMaxScrollBack() int {
	bodyHeight := m.quickLookBodyHeight()
	if !m.paneView {
		return max(0, len(m.previewLayout)-bodyHeight)
	}
	if !m.paneLoaded {
		return 0
	}
	history := min(m.paneCursor.History, len(m.paneLines))
	screenRows := len(m.paneLines) - history
	screenOverflow := max(0, screenRows-bodyHeight)
	return max(0, m.paneCursor.HistorySize+screenOverflow)
}

func (m *Model) setPreviewScrollBack(offset int) {
	m.previewScrollBack = max(0, min(offset, m.previewMaxScrollBack()))
}

func (m *Model) clampPreviewScrollBack() {
	m.setPreviewScrollBack(m.previewScrollBack)
}

// paneScrollRefresh retires the live poll when reading leaves the bottom,
// expands the captured history only when the requested viewport needs it, and
// restarts the live poll as soon as the reader returns. A scrolled terminal
// staying still while output continues is the familiar copy-mode contract —
// and avoids recapturing thousands of history rows several times a second.
func (m *Model) paneScrollRefresh(before int) tea.Cmd {
	if !m.paneView {
		return nil
	}
	crossedLiveEdge := (before > 0) != (m.previewScrollBack > 0)
	history := m.paneHistoryRequest()
	needsMoreHistory := history > m.paneHistoryRequested
	if !crossedLiveEdge && !needsMoreHistory {
		return nil
	}
	session := m.previewedSession()
	if session == nil {
		return nil
	}
	// A capture already in flight answers for the other side of the edge, and
	// landing late it would replace the history someone is reading with a
	// bare screen until the next tick. Retiring the generation drops it and the
	// poll chain with it; a reload at the live bottom starts that chain again.
	m.previewGeneration++
	if m.previewScrollBack > 0 && !needsMoreHistory {
		return nil
	}
	return m.loadPane(*session, m.previewGeneration, true)
}

// handleClick resolves a click against the zones the last frame recorded.
// Whatever floats above the board — Quick Look, the detail card, the help
// footer — is dealt with first: a click outside a floating thing is the
// mouse's way of putting it down, mirroring esc.
func (m *Model) handleClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseLeft {
		return m, nil
	}
	if m.previewOpen {
		if !m.quickLookRect.contains(msg.X, msg.Y) {
			m.previewOpen = false
			m.paneInput = false
		}
		return m, nil
	}
	if m.helpOpen {
		m.helpOpen = false
		return m, nil
	}
	if m.detail {
		if !m.detailRect.contains(msg.X, msg.Y) {
			m.detail = false
		}
		return m, nil
	}
	zone, ok := m.zoneAt(msg.X, msg.Y)
	if !ok {
		return m, nil
	}
	// A click is aimed at what is on screen, so whichever input owns the
	// keyboard is put down first — the way esc would, keeping the search query
	// as a filter and the composer's draft for later.
	if m.searching && zone.action != clickSearch {
		m.searching = false
	}
	if m.composing && zone.action != clickComposer {
		m.composing = false
	}
	switch zone.action {
	case clickGroupStatus:
		m.setGroup(groupStatus)
	case clickGroupProjects:
		m.setGroup(groupProject)
	case clickLayoutKanban:
		m.setLayout(layoutKanban)
	case clickLayoutList:
		m.setLayout(layoutList)
		return m, m.startAnimIfNeeded()
	case clickSearch:
		m.searching = true
	case clickAgentChip:
		m.toggleAgentAt(zone.column)
	case clickComposer:
		if m.canCompose() {
			m.composing = true
		}
	case clickColumnTab:
		m.column = zone.column
		m.row = 0
		m.clampSelection()
	case clickColumnPrev:
		m.moveColumn(-1)
	case clickColumnNext:
		m.moveColumn(1)
	case clickCard:
		// The first click brings the cursor over; a second on the same card is
		// a request to see it, the way space is from the keyboard.
		already := m.column == zone.column && m.row == zone.row
		m.column, m.row = zone.column, zone.row
		if already {
			return m, m.openQuickLook()
		}
	}
	return m, nil
}

func (m *Model) zoneAt(x, y int) (clickZone, bool) {
	for _, zone := range m.clickZones {
		if zone.rect.contains(x, y) {
			return zone, true
		}
	}
	return clickZone{}, false
}

func (m *Model) addClickZone(zone clickZone) {
	m.clickZones = append(m.clickZones, zone)
}

func (m *Model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	stroke := key.Keystroke()
	// Any keystroke other than the confirming ctrl+x stands a pending
	// dismissal down: the second press must mean nothing but "yes".
	if stroke != "ctrl+x" {
		m.pendingDismissID = ""
	}
	if stroke == "ctrl+s" {
		m.searching = false
		m.composing = false
		m.detail = false
		m.helpOpen = false
		m.previewOpen = false
		m.toggleGroup()
		return m, nil
	}
	if m.searching {
		switch stroke {
		case "esc":
			m.searching = false
		case "enter":
			m.searching = false
			m.column, m.row = 0, 0
			m.clampSelection()
		case "backspace":
			if len(m.query) > 0 {
				_, size := utf8.DecodeLastRuneInString(m.query)
				m.query = m.query[:len(m.query)-size]
			}
		case "ctrl+u":
			m.query = ""
		default:
			if key.Key().Text != "" {
				m.query += key.Key().Text
			}
		}
		return m, nil
	}

	// The composer owns the whole keyboard while it is focused: a task
	// description is ordinary text, and "q" in one must not quit the board.
	if m.composing {
		active := m.composeMentionActive()
		menu := m.composeMenuEntries()
		switch stroke {
		case "esc":
			if active {
				// The @ was literal text after all; the menu steps aside and
				// the next esc puts the composer down as usual.
				m.composeMenuHidden = true
			} else {
				m.composing = false
			}
		case "enter":
			if active {
				// While an @ is being completed, enter means "take the pick"
				// and nothing else. With no match it does nothing at all —
				// starting the session now would ship the typo'd token as
				// prompt text and the task to the wrong directory; esc is
				// the deliberate way to keep a literal @.
				if len(menu) > 0 {
					m.acceptComposeMention(menu)
				}
				return m, nil
			}
			return m, m.startComposedSession()
		case "tab":
			if active {
				if len(menu) > 0 {
					m.completeComposeMention(menu)
				}
			} else {
				m.composeAgent = (m.composeAgent + 1) % len(m.launchers)
			}
		case "up":
			if len(menu) > 0 {
				m.composeMenuSel = (m.composeMenuSel - 1 + len(menu)) % len(menu)
			}
		case "down":
			if len(menu) > 0 {
				m.composeMenuSel = (m.composeMenuSel + 1) % len(menu)
			}
		case "backspace":
			if len(m.composeText) > 0 {
				_, size := utf8.DecodeLastRuneInString(m.composeText)
				m.composeText = m.composeText[:len(m.composeText)-size]
				m.composeTextEdited()
			}
		case "ctrl+u":
			m.composeText = ""
			m.composeTextEdited()
		default:
			if key.Key().Text != "" {
				m.composeText += key.Key().Text
				m.composeTextEdited()
			}
		}
		return m, nil
	}

	if m.previewOpen && m.paneInput {
		switch stroke {
		case "esc", paneEscapeKey:
			// Esc leaves typing, nothing more: the window stays up, back in
			// browse mode where space closes it. The cost is that esc cannot
			// be typed at the agent — ctrl+c still reaches it for interrupts.
			m.paneInput = false
			return m, nil
		}
		// Everything else — space included — is typing and goes to the agent.
		return m, m.sendToPane(key)
	}

	if m.previewOpen {
		switch stroke {
		case "ctrl+c", "ctrl+q", "q":
			return m, tea.Quit
		case "t":
			return m, m.togglePaneView()
		case "i":
			if m.paneView {
				m.paneInput = true
				// The poll is running at the slower interval; restart it so the
				// first keystroke does not wait out the tick already in flight.
				m.previewGeneration++
				if session := m.previewedSession(); session != nil {
					return m, m.loadPane(*session, m.previewGeneration, true)
				}
			}
		case "esc", " ", "space":
			m.previewOpen = false
		case "up", "k":
			return m, m.scrollPreviewBy(1)
		case "down", "j":
			return m, m.scrollPreviewBy(-1)
		case "pgup":
			return m, m.scrollPreviewBy(max(1, m.quickLookBodyHeight()-2))
		case "pgdown":
			return m, m.scrollPreviewBy(-max(1, m.quickLookBodyHeight()-2))
		case "g":
			before := m.previewScrollBack
			m.setPreviewScrollBack(m.previewMaxScrollBack())
			return m, m.paneScrollRefresh(before)
		case "G":
			if !m.paneView && m.previewTranscriptReturnsToPane {
				return m, m.returnToLivePane()
			}
			before := m.previewScrollBack
			m.setPreviewScrollBack(0)
			return m, m.paneScrollRefresh(before)
		case "d":
			m.previewOpen = false
			m.detail = true
		case "enter":
			m.previewOpen = false
			return m, m.openSelectedInTab()
		}
		return m, nil
	}

	if m.helpOpen {
		switch stroke {
		case "ctrl+c", "ctrl+q", "q":
			return m, tea.Quit
		case "?", "esc":
			m.helpOpen = false
		}
		return m, nil
	}

	if m.detail {
		if stroke == "ctrl+c" || stroke == "ctrl+q" || stroke == "q" {
			return m, tea.Quit
		}
		if stroke == "esc" || stroke == "d" || stroke == " " ||
			stroke == "space" || stroke == "enter" {
			m.detail = false
		}
		return m, nil
	}

	switch stroke {
	case "ctrl+c", "ctrl+q", "q":
		// The two modified strokes are not spare spellings of q: an input
		// method that composes text out of the letter keys swallows q whole,
		// and switching it off to leave the board is a step no one should
		// have to take. ctrl is never composed, so it always reaches here.
		return m, tea.Quit
	case "tab":
		m.toggleGroup()
	case "v":
		m.toggleLayout()
		return m, m.startAnimIfNeeded()
	case "?":
		m.helpOpen = true
	case "/", "s", "ctrl+k":
		// ctrl+k is the terminal's reach for the cmd+k every command palette
		// taught; cmd itself never gets past the terminal emulator.
		m.searching = true
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// The digits toggle the header's chips in the order they are drawn,
		// so the keyboard reaches the same independent switches the mouse
		// does, rather than a cycle that could only ever pick one agent.
		m.toggleAgentAt(int(stroke[0] - '1'))
	case "a":
		m.clearAgentFilter()
	case "n":
		if m.canCompose() {
			m.composing = true
		}
	case "esc":
		if m.query != "" {
			m.query = ""
			m.column, m.row = 0, 0
			m.clampSelection()
		}
	case "left", "h":
		m.moveColumn(-1)
	case "right", "l":
		m.moveColumn(1)
	case "up", "k":
		m.moveRow(-1)
	case "down", "j":
		m.moveRow(1)
	case "d":
		if m.selected() != nil {
			m.detail = true
		}
	case " ", "space", "enter":
		// Quick Look is the board's main interaction: enter goes where the
		// eye already went. Going to the session itself is the heavier,
		// rarer act — ctrl+enter here, or enter from inside Quick Look.
		return m, m.openQuickLook()
	case "r":
		m.loading = true
		return m, m.refreshCmd()
	case "ctrl+x":
		m.dismissSelected()
	case "ctrl+enter":
		return m, m.openSelectedInTab()
	}
	return m, nil
}

// dismissSelected takes the selected session off the board on the second
// ctrl+x, remembered in the board's own state rather than the agent's. It
// works for every agent and every status: it changes what the board shows,
// not what the agent has.
func (m *Model) dismissSelected() {
	selected := m.selected()
	if selected == nil {
		return
	}
	if m.dismissals == nil {
		m.status = "Dismiss unavailable: the dismissal state file could not be read"
		return
	}
	armed := m.pendingDismissID == selected.ID &&
		time.Since(m.pendingDismissAt) <= dismissConfirmWindow
	if !armed {
		m.pendingDismissID = selected.ID
		m.pendingDismissAt = time.Now()
		m.status = "Press ctrl+x again to dismiss this session"
		return
	}
	m.pendingDismissID = ""
	if err := m.dismissals.Dismiss(selected.Agent, selected.ID); err != nil {
		m.status = "Dismiss failed: " + err.Error()
		return
	}
	m.status = "Session dismissed"
	m.clampSelection()
}

func (m *Model) openQuickLook() tea.Cmd {
	selected := m.selected()
	if selected == nil {
		return nil
	}
	session := *selected
	m.previewOpen = true
	m.previewLoading = true
	m.previewGeneration++
	m.previewSessionID = session.ID
	m.previewSession = session
	m.previewMessages = nil
	m.previewStatus = ""
	m.previewActivity = agent.Activity{}
	m.previewErr = nil
	m.previewScrollBack = 0
	m.previewMessageLimit = previewMessagePage
	m.previewLoadedLimit = 0
	m.previewTranscriptExhausted = false
	m.previewTranscriptLoadingMore = false
	m.previewPendingScrollBack = 0
	m.previewTranscriptReturnsToPane = false
	m.previewReturnPaneInput = false
	m.paneLines = nil
	m.paneLoaded = false
	m.paneHistoryRequested = 0
	m.paneAnchor = 0
	m.paneWidth = 0
	m.paneScreenWidth = 0
	m.paneErr = nil
	// A session running in a pane has a screen, and the screen is the better
	// answer: it shows the prompt the agent is blocked on, which never reaches
	// the rollout log at all.
	m.paneView = m.canMirrorPane(session)
	// The window opens looking, not typing, so the space bar that opened it
	// closes it again — the macOS Quick Look rhythm. Answering the agent is
	// one i away, and the footer says so.
	m.paneInput = false
	// The board behind the window is captured after the view is decided, so the
	// backdrop is cut for the window that is actually about to be drawn.
	m.setPreviewBase(m.renderBase())
	m.rebuildPreviewLayout()
	m.previewAnimFrom = m.selectedRect
	m.previewOpenedAt = time.Now()
	m.previewAnimGeneration++
	m.previewAnimating = true
	load := m.loadPreview(session, m.previewGeneration)
	if m.paneView {
		load = m.loadPane(session, m.previewGeneration, true)
	}
	return tea.Batch(load, m.previewAnimTick())
}

func (m *Model) previewAnimTick() tea.Cmd {
	generation := m.previewAnimGeneration
	return tea.Tick(previewOpenFrameInterval, func(time.Time) tea.Msg {
		return previewAnimMsg{generation: generation}
	})
}

func (m *Model) canMirrorPane(session agent.Session) bool {
	return m.panes != nil && session.TmuxPane != ""
}

// togglePaneView swaps between the live pane and the stored transcript, and
// starts whichever poll the new side needs.
func (m *Model) togglePaneView() tea.Cmd {
	session := m.previewedSession()
	if session == nil || !m.canMirrorPane(*session) {
		return nil
	}
	m.paneView = !m.paneView
	// Switching views is a way of looking, not an intent to speak: typing
	// stays behind i on the pane side too.
	m.paneInput = false
	m.previewLoading = true
	m.previewScrollBack = 0
	m.previewMessageLimit = previewMessagePage
	m.previewLoadedLimit = 0
	m.previewTranscriptExhausted = false
	m.previewTranscriptLoadingMore = false
	m.previewPendingScrollBack = 0
	m.previewTranscriptReturnsToPane = false
	m.previewReturnPaneInput = false
	m.paneLoaded = false
	m.paneHistoryRequested = 0
	m.paneAnchor = 0
	m.previewGeneration++
	if m.paneView {
		return m.loadPane(*session, m.previewGeneration, true)
	}
	m.previewMessages = nil
	m.rebuildPreviewLayout()
	return m.loadPreview(*session, m.previewGeneration)
}

// sendToPane forwards one keystroke to the mirrored pane. Text and named keys
// go in separate tmux calls, which is also what keeps a typed line intact:
// agent TUIs treat a paste and a newline behind it as one event and drop the
// newline, so the return key is never batched with the characters before it.
func (m *Model) sendToPane(key tea.KeyPressMsg) tea.Cmd {
	session := m.previewedSession()
	if session == nil || !m.canMirrorPane(*session) {
		return nil
	}
	if text := key.Key().Text; text != "" {
		return m.sendPaneText(text)
	}
	name, ok := paneKeyName(key.Keystroke())
	if !ok {
		return nil
	}
	return m.sendPaneKey(name)
}

// sendPaneKey sends one named tmux key to the mirrored pane.
func (m *Model) sendPaneKey(name string) tea.Cmd {
	session := m.previewedSession()
	if session == nil || !m.canMirrorPane(*session) {
		return nil
	}
	pane := session.TmuxPane
	panes := m.panes
	generation := m.previewGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return paneSentMsg{
			generation: generation,
			err:        panes.SendKey(ctx, pane, name),
		}
	}
}

func (m *Model) sendPaneText(text string) tea.Cmd {
	session := m.previewedSession()
	if session == nil || !m.canMirrorPane(*session) {
		return nil
	}
	pane := session.TmuxPane
	panes := m.panes
	generation := m.previewGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return paneSentMsg{
			generation: generation,
			err:        panes.SendText(ctx, pane, text),
		}
	}
}

// paneKeyName translates a keystroke into the name tmux send-keys expects.
// Unknown keystrokes are dropped rather than guessed at: sending the wrong key
// to a live agent is worse than sending nothing.
func paneKeyName(stroke string) (string, bool) {
	named := map[string]string{
		"esc":       "Escape",
		"enter":     "Enter",
		"backspace": "BSpace",
		"tab":       "Tab",
		"shift+tab": "BTab",
		"up":        "Up",
		"down":      "Down",
		"left":      "Left",
		"right":     "Right",
		"home":      "Home",
		"end":       "End",
		"pgup":      "PageUp",
		"pgdown":    "PageDown",
		"delete":    "DC",
		"insert":    "IC",
		"space":     "Space",
		" ":         "Space",
	}
	if name, ok := named[stroke]; ok {
		return name, true
	}
	// ctrl+<letter> maps straight onto tmux's C-<letter>, which is how an agent
	// gets interrupted (ctrl+c) or a line cleared (ctrl+u) from the board.
	if rest, ok := strings.CutPrefix(stroke, "ctrl+"); ok &&
		len(rest) == 1 && rest[0] >= 'a' && rest[0] <= 'z' {
		return "C-" + rest, true
	}
	return "", false
}

func (m *Model) paneInterval() time.Duration {
	if m.paneInput {
		return paneInputRefreshInterval
	}
	return paneRefreshInterval
}

// paneHistoryPageLines is a starting page, not a ceiling. Captures double from
// here as the reader moves backwards until they reach the pane's real
// history_size. That keeps ordinary scrolling cheap without silently hiding a
// 50,000-line history behind tmux's old 2,000-line default.
const paneHistoryPageLines = 512

func (m *Model) paneHistoryRequest() int {
	if !m.paneLoaded ||
		m.previewScrollBack <= 0 ||
		m.paneCursor.HistorySize <= 0 {
		return 0
	}
	history := min(m.paneCursor.History, len(m.paneLines))
	screenRows := len(m.paneLines) - history
	screenOverflow := max(0, screenRows-m.quickLookBodyHeight())
	needed := max(0, m.previewScrollBack-screenOverflow)
	available := max(history, m.paneHistoryRequested)
	if needed <= available {
		return available
	}
	target := max(needed, paneHistoryPageLines)
	if available > 0 {
		target = max(target, available*2)
	}
	return min(target, m.paneCursor.HistorySize)
}

func (m *Model) loadPane(
	session agent.Session,
	generation uint64,
	poll bool,
) tea.Cmd {
	panes := m.panes
	history := m.paneHistoryRequest()
	m.paneHistoryRequested = history
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		screen, err := panes.Capture(ctx, session.TmuxPane, history)
		return paneLoadedMsg{
			sessionID:  session.ID,
			generation: generation,
			screen:     screen,
			err:        err,
			poll:       poll,
		}
	}
}

func (m *Model) loadPreview(session agent.Session, generation uint64) tea.Cmd {
	adapter := m.adapter
	limit := max(previewMessagePage, m.previewMessageLimit)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		transcript, err := adapter.Preview(ctx, session, limit)
		return previewLoadedMsg{
			sessionID:  session.ID,
			generation: generation,
			limit:      limit,
			transcript: transcript,
			err:        err,
		}
	}
}

// previewedSession returns the session the overlay was opened on. It is kept
// separate from the board's list because that list stops being refreshed while
// the overlay is up, and the session can be filtered out from under it.
func (m *Model) previewedSession() *agent.Session {
	if !m.previewOpen || m.previewSessionID == "" {
		return nil
	}
	return &m.previewSession
}

// previewLive reports whether the transcript itself shows an open turn, which
// is what makes the overlay worth watching.
func (m *Model) previewLive() bool {
	return m.previewStatus == agent.StatusRunning ||
		m.previewStatus == agent.StatusNeedsYou
}

// previewDisplayStatus prefers what the transcript says over the status the
// board recorded when the overlay opened, since discovery is paused behind it.
// Archived is a board fact a transcript cannot contradict.
func (m *Model) previewDisplayStatus(session agent.Session) agent.RuntimeStatus {
	if session.RuntimeStatus == agent.StatusArchived || m.previewStatus == "" {
		return session.RuntimeStatus
	}
	return m.previewStatus
}

// previewActivityLine describes work happening past the last visible message,
// so that a long turn full of tool calls does not read as a frozen transcript.
// The age is always shown: a session whose process died mid-turn keeps its open
// turn forever, and a stale timestamp is what gives that away.
func (m *Model) previewActivityLine() string {
	if m.previewErr != nil || m.previewLoading {
		return ""
	}
	var label string
	switch m.previewStatus {
	case agent.StatusNeedsYou:
		label = "● needs you"
	case agent.StatusRunning:
		label = "● working"
	default:
		return ""
	}
	if m.previewActivity.Label != "" {
		label += " · " + m.previewActivity.Label
	}
	if age := activityAge(m.previewActivity.At); age != "" {
		label += " · " + age
	}
	return label
}

func activityAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func previewChanged(
	current []agent.TranscriptMessage,
	currentErr error,
	next []agent.TranscriptMessage,
	nextErr error,
) bool {
	if errorText(currentErr) != errorText(nextErr) || len(current) != len(next) {
		return true
	}
	for i := range current {
		if current[i] != next[i] {
			return true
		}
	}
	return false
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// agentNames is the run of agents the header offers chips for: the ones
// discovery actually found, plus any the filter still names so a chip never
// disappears out from under the selection that would clear it.
func (m *Model) agentNames() []string {
	seen := map[string]bool{}
	for _, session := range m.sessions {
		if name := strings.ToLower(session.Agent); name != "" {
			seen[name] = true
		}
	}
	for name := range m.agentFilter {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// filteredAgents names the lit chips the way the chips themselves do, for the
// empty board to say whose sessions it is holding back.
func (m *Model) filteredAgents() []string {
	names := make([]string, 0, len(m.agentFilter))
	for name := range m.agentFilter {
		names = append(names, strings.ToUpper(name))
	}
	sort.Strings(names)
	return names
}

// heldBackByAgentFilter counts the cards the board would be drawing with no
// chip lit — the size of what the filter, rather than the day's quiet, is
// keeping off an empty board. It counts cards rather than visible sessions
// because the two differ: an archived session passes sessionVisible but no
// column takes it, and promising a card that clearing the filter will not
// produce is worse than saying nothing.
func (m *Model) heldBackByAgentFilter() int {
	filter := m.agentFilter
	m.agentFilter = nil
	defer func() { m.agentFilter = filter }()
	held := 0
	for columnIndex := range m.columns() {
		held += len(m.cardsForColumn(columnIndex))
	}
	return held
}

// toggleAgentFilter turns one agent's chip on or off. Emptying the set is the
// same as never having filtered: the board goes back to every agent.
func (m *Model) toggleAgentFilter(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return
	}
	// Filtering redraws the columns under the cursor, so the selection is kept
	// by session rather than by place: a card still on the board after the
	// toggle is the card the next enter opens, wherever the redraw put it.
	selected := ""
	if session := m.selected(); session != nil {
		selected = session.ID
	}
	if m.agentFilter[name] {
		delete(m.agentFilter, name)
	} else {
		if m.agentFilter == nil {
			m.agentFilter = map[string]bool{}
		}
		m.agentFilter[name] = true
	}
	m.restoreSelection(selected)
	m.savePrefs()
}

// toggleAgentAt names the chip a key or a click aimed at by its place in the
// header, out of range being a key pressed at a chip that is not there.
func (m *Model) toggleAgentAt(index int) {
	names := m.agentNames()
	if index < 0 || index >= len(names) {
		return
	}
	m.toggleAgentFilter(names[index])
}

func (m *Model) clearAgentFilter() {
	if len(m.agentFilter) == 0 {
		return
	}
	selected := ""
	if session := m.selected(); session != nil {
		selected = session.ID
	}
	m.agentFilter = nil
	m.restoreSelection(selected)
	m.savePrefs()
}

func (m *Model) toggleGroup() {
	m.setGroup((m.group + 1) % boardGroupCount)
}

// setGroup is toggleGroup aimed at a named tab: clicking the one already lit
// changes nothing, so the selection is only reset when the grouping is.
func (m *Model) setGroup(group boardGroup) {
	if m.group == group {
		return
	}
	m.group = group
	m.column, m.row = 0, 0
	m.clampSelection()
	m.savePrefs()
}

// toggleLayout redraws the same groups the other way, so the selection is a
// place worth keeping rather than resetting.
func (m *Model) toggleLayout() {
	if m.layout == layoutKanban {
		m.setLayout(layoutList)
	} else {
		m.setLayout(layoutKanban)
	}
}

func (m *Model) setLayout(layout boardLayout) {
	if m.layout == layout {
		return
	}
	m.layout = layout
	m.clampSelection()
	m.savePrefs()
}

func (m *Model) moveColumn(delta int) {
	count := len(m.columns())
	if count == 0 || delta == 0 {
		return
	}
	direction := 1
	if delta < 0 {
		direction = -1
	}
	for step := 1; step <= count; step++ {
		candidate := (m.column + direction*step + count) % count
		if len(m.cardsForColumn(candidate)) == 0 {
			continue
		}
		m.column = candidate
		m.row = 0
		m.clampSelection()
		return
	}
}

func (m *Model) moveRow(delta int) {
	if m.layout == layoutList {
		m.moveListRow(delta)
		return
	}
	cards := m.cardsForColumn(m.column)
	if len(cards) == 0 {
		m.row = 0
		return
	}
	m.row = (m.row + delta + len(cards)) % len(cards)
}

// moveListRow walks the list as one run of sessions. Groups are headings there
// rather than places to be, so the end of one leads into the next instead of
// wrapping back to its own top.
func (m *Model) moveListRow(delta int) {
	type position struct{ column, row int }
	var flat []position
	current := -1
	for columnIndex := range m.columns() {
		for rowIndex := range m.cardsForColumn(columnIndex) {
			if columnIndex == m.column && rowIndex == m.row {
				current = len(flat)
			}
			flat = append(flat, position{column: columnIndex, row: rowIndex})
		}
	}
	if len(flat) == 0 {
		m.column, m.row = 0, 0
		return
	}
	next := 0
	switch {
	case current >= 0:
		next = (current + delta + len(flat)) % len(flat)
	case delta < 0:
		next = len(flat) - 1
	}
	m.column, m.row = flat[next].column, flat[next].row
}

// canCompose reports whether the standing input can do anything at all, which
// needs both somewhere to run a session and an agent to run in it.
func (m *Model) canCompose() bool {
	return m.starter != nil && len(m.launchers) > 0
}

// composeDirs is everywhere a composed task can start: the directory the
// board was launched from, then every project a session already ran in,
// freshest first. Like the Projects grouping, the list is derived from the
// sessions on the board rather than configured — the places work happens are
// already known, and a brand-new one is a cd away.
func (m *Model) composeDirs() []string {
	base := filepath.Clean(m.workdir)
	latest := map[string]time.Time{}
	for _, session := range m.sessions {
		path := filepath.Clean(session.CWD)
		if path == "." || path == base {
			continue
		}
		if session.UpdatedAt.After(latest[path]) {
			latest[path] = session.UpdatedAt
		}
	}
	dirs := make([]string, 0, len(latest)+1)
	dirs = append(dirs, base)
	for path := range latest {
		dirs = append(dirs, path)
	}
	sort.SliceStable(dirs[1:], func(i, j int) bool {
		return latest[dirs[1+i]].After(latest[dirs[1+j]])
	})
	return dirs
}

// currentComposeDir is composeDirs' selection resolved to a path, falling
// back to the board's own working directory when nothing was picked or the
// picked project's sessions have since left the board.
func (m *Model) currentComposeDir() string {
	if m.composeDir != "" {
		return m.composeDir
	}
	return m.workdir
}

// composeMenuMax bounds the completion menu: enough rows to pick from
// without the board disappearing behind them.
const composeMenuMax = 6

// minBoardHeight is the floor the board area never gives up, menu or not —
// composeMenuCap trims the menu against it so the two floors cannot add up
// past the bottom of the terminal.
const minBoardHeight = 5

// A card's proportions, in terminal cells: four lines of content — meta, two
// title rows, one status row — and the border around them, with one row
// between cards. The title always gets its second row, blank when unneeded, so
// every card is the same height and a column reflows without measuring.
const (
	cardHeight = 6
	cardStride = cardHeight + 1
	// compactCardHeight is the same card with the gap after it given up, for
	// columns too short to hold even one card and its breathing room.
	compactCardHeight = cardHeight
	cardChrome        = 4 // two border columns and one of padding on each side
	maxColumnWidth    = 38
)

// composeTextEdited is every edit's bookkeeping: the entries under the menu
// changed, so the highlight and a standing dismissal are both stale.
func (m *Model) composeTextEdited() {
	m.composeMenuSel = 0
	m.composeMenuHidden = false
}

// composeMention is the @token being typed at the end of the composer: the
// trailing run of non-space text when it opens with an @ that starts a word.
// The composer only ever edits at its end, so the token under the cursor is
// always the last one. An @ inside a word — an email address — is not a
// mention. Spaces escaped shell-style (\ ) stay inside the token, which is
// how a completed "My Project" survives to the next keystroke; the returned
// query has the escapes resolved.
func composeMention(text string) (start int, query string, ok bool) {
	cut := 0
	for i := len(text) - 1; i >= 0; i-- {
		if (text[i] == ' ' || text[i] == '\t') && (i == 0 || text[i-1] != '\\') {
			cut = i + 1
			break
		}
	}
	token := text[cut:]
	if !strings.HasPrefix(token, "@") {
		return 0, "", false
	}
	return cut, strings.ReplaceAll(token[1:], `\ `, " "), true
}

// composeMentionActive reports whether the composer is inside an @token the
// menu should answer for — even one with no matches, since enter changing
// meaning on an empty result would ship a typo to the wrong directory.
func (m *Model) composeMentionActive() bool {
	if !m.composing || m.composeMenuHidden {
		return false
	}
	_, _, ok := composeMention(m.composeText)
	return ok
}

// composeMenuCap is how many rows the menu may take on this terminal: up to
// composeMenuMax, less whatever would push the board below its own minimum
// height and the whole layout past the bottom of the screen.
func (m *Model) composeMenuCap() int {
	room := m.height - 4 - m.footerHeight() - 2 - minBoardHeight
	return max(1, min(composeMenuMax, room))
}

// composeMenuEntries is what the @ being typed could mean, in the order the
// menu shows them: projects off the board matched by name, or directories on
// disk when the query reads as a path.
func (m *Model) composeMenuEntries() []string {
	if !m.composeMentionActive() {
		return nil
	}
	_, query, _ := composeMention(m.composeText)
	limit := m.composeMenuCap()
	if strings.HasPrefix(query, "/") || strings.HasPrefix(query, "~") {
		return completeDirs(query, limit)
	}
	needle := strings.ToLower(query)
	var entries []string
	for _, dir := range m.composeDirs() {
		if strings.Contains(strings.ToLower(projectName(dir)), needle) ||
			strings.Contains(strings.ToLower(dir), needle) {
			entries = append(entries, dir)
		}
		if len(entries) == limit {
			break
		}
	}
	return entries
}

// completeDirs lists the directories a partial path could continue as, the
// way a shell completes one: entries of the query's parent whose names start
// with its last element, and the query's own directory first when it already
// names one, so a fully typed path is accepted rather than completed further.
func completeDirs(query string, limit int) []string {
	expanded, ok := expandHome(query)
	if !ok {
		return nil
	}
	parent, prefix := filepath.Dir(expanded), filepath.Base(expanded)
	var entries []string
	if strings.HasSuffix(expanded, "/") {
		parent, prefix = expanded, ""
		if info, err := os.Stat(expanded); err == nil && info.IsDir() {
			entries = append(entries, filepath.Clean(expanded))
		}
	}
	if len(entries) >= limit {
		return entries
	}
	listed, err := os.ReadDir(parent)
	if err != nil {
		return entries
	}
	for _, entry := range listed {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Dotdirs stay out of the way until asked for by their dot.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		full := filepath.Join(parent, name)
		if !entry.IsDir() {
			// A symlink to a directory completes like the directory.
			if info, err := os.Stat(full); err != nil || !info.IsDir() {
				continue
			}
		}
		entries = append(entries, full)
		if len(entries) >= limit {
			break
		}
	}
	return entries
}

// expandHome resolves a leading ~ the way a shell does, and only the way a
// shell does: bare ~ and ~/, not ~user.
func expandHome(path string) (string, bool) {
	if !strings.HasPrefix(path, "~") {
		return path, true
	}
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	if path == "~" {
		return home + "/", true
	}
	return home + path[1:], true
}

// abbreviateHome is expandHome's inverse for display: paths under the home
// directory read as ~/… so the menu spends its width on what differs.
func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}

// acceptComposeMention resolves the @token to the highlighted entry: the task
// will start there, and the token leaves the text — the directory is the
// task's address, not part of its description.
func (m *Model) acceptComposeMention(entries []string) {
	start, _, ok := composeMention(m.composeText)
	if !ok {
		return
	}
	m.composeDir = filepath.Clean(entries[min(m.composeMenuSel, len(entries)-1)])
	m.composeText = m.composeText[:start]
	m.composeTextEdited()
}

// completeComposeMention is tab on the menu. A path query completes shell
// style — the highlighted directory fills the token, open at its end for
// drilling deeper — while a project query has nothing deeper to drill, so
// tab accepts it outright.
func (m *Model) completeComposeMention(entries []string) {
	start, query, ok := composeMention(m.composeText)
	if !ok {
		return
	}
	if !strings.HasPrefix(query, "/") && !strings.HasPrefix(query, "~") {
		m.acceptComposeMention(entries)
		return
	}
	completed := entries[min(m.composeMenuSel, len(entries)-1)]
	if strings.HasPrefix(query, "~") {
		completed = abbreviateHome(completed)
	}
	// Spaces go back escaped, or the next keystroke's token parse would cut
	// the path at "My Project"'s gap and the mention would fall apart.
	completed = strings.ReplaceAll(completed, " ", `\ `)
	m.composeText = m.composeText[:start] + "@" + completed + "/"
	m.composeTextEdited()
}

// composedPaneSize is the size a composed session is created at. tmux gives a
// detached session 80×24 unless told otherwise, which mirrored as a small
// window adrift in a mostly empty overlay. Until a client attaches, the
// mirror is the pane's only viewer, so the pane is cut to the largest screen
// the roomy floating frame shows whole — the first real attach brings its own
// size and tmux resizes the window to that client.
func (m *Model) composedPaneSize() (width, height int) {
	width = max(80, m.width-roomyPaneFrame.chrome-2*roomyPaneFrame.margin)
	// Vertical margins plus the eight rows quickLookBodyHeight spends on the
	// floating window's border, header, rules and footer.
	height = max(24, m.height-2*paneWindowVMargin-8)
	return width, height
}

// startComposedSession starts the described task as a fresh agent in a tmux
// session of its own. The board takes it from there: discovery finds the new
// session, and Quick Look can mirror it like any other pane.
func (m *Model) startComposedSession() tea.Cmd {
	prompt := strings.TrimSpace(m.composeText)
	if prompt == "" {
		// Enter on an empty composer is the other way of putting it down.
		m.composing = false
		return nil
	}
	if !m.canCompose() {
		return nil
	}
	launcher := m.launchers[m.composeAgent]
	name, args := launcher.Command(prompt)
	command := append([]string{name}, args...)
	sessionName := tmux.SessionName(prompt)
	starter := m.starter
	dir := m.currentComposeDir()
	agentName := launcher.Agent
	width, height := m.composedPaneSize()
	m.composing = false
	m.composeText = ""
	m.status = "Starting " + agentName + "…"
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		created, err := starter.NewSession(
			ctx, sessionName, dir, command, width, height,
		)
		return sessionStartedMsg{
			agent:  agentName,
			name:   created,
			prompt: prompt,
			err:    err,
		}
	}
}

// openSelectedInTab takes the selected session to a new tab of the
// surrounding terminal: attach when it lives in a tmux pane, the agent's own
// resume otherwise — either way the board keeps its window, which is what
// makes it a console rather than a launcher. Terminals the board cannot ask
// for a tab fall back to the classic hand-over of this window.
func (m *Model) openSelectedInTab() tea.Cmd {
	selected := m.selected()
	if selected == nil || selected.Archived {
		return nil
	}
	if selected.PID != 0 && selected.TmuxPane == "" {
		m.status = "Session is already open in another terminal"
		return nil
	}
	if m.opener == nil {
		return m.resumeSelected()
	}
	// Only a resume runs in the session's directory. An attach cares about
	// the pane, not the path — a workspace renamed or unmounted since must
	// not stop a perfectly attachable session at the cd.
	dir := selected.CWD
	var command []string
	if selected.TmuxPane != "" {
		dir = ""
		// A fresh tab's shell is never inside tmux, so the way in is always
		// attach — never switch-client — with the session's window and pane
		// selected first so the agent is what lands on screen.
		pane := selected.TmuxPane
		command = []string{
			"tmux",
			"select-window", "-t", pane,
			";", "select-pane", "-t", pane,
			";", "attach", "-t", pane,
		}
	} else {
		name, args := m.adapter.ResumeCommand(*selected)
		command = append([]string{name}, args...)
	}
	opener := m.opener
	m.status = "Opening in a new tab…"
	return func() tea.Msg {
		return tabOpenedMsg{err: opener.OpenTab(dir, command)}
	}
}

func (m *Model) resumeSelected() tea.Cmd {
	selected := m.selected()
	if selected == nil || selected.Archived {
		return nil
	}
	// A session running in a pane can be returned to; one running in some other
	// terminal cannot, and resuming it would start a second agent against the
	// same rollout.
	if selected.PID != 0 && selected.TmuxPane == "" {
		m.status = "Session is already open in another terminal"
		return nil
	}
	name, args := m.adapter.ResumeCommand(*selected)
	cmd := exec.Command(name, args...)
	cmd.Dir = selected.CWD
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return resumeFinishedMsg{err: err}
	})
}

func (m *Model) selected() *agent.Session {
	cards := m.cardsForColumn(m.column)
	if m.row < 0 || m.row >= len(cards) {
		return nil
	}
	selectedID := cards[m.row].ID
	for i := range m.sessions {
		if m.sessions[i].ID == selectedID {
			return &m.sessions[i]
		}
	}
	return nil
}

func (m *Model) clampSelection() {
	columns := m.columns()
	if len(columns) == 0 {
		m.column, m.row = 0, 0
		return
	}
	if m.column >= len(columns) {
		m.column = len(columns) - 1
	}
	if m.column < 0 {
		m.column = 0
	}
	if len(m.cardsForColumn(m.column)) == 0 {
		for step := 1; step <= len(columns); step++ {
			candidate := (m.column + step) % len(columns)
			if len(m.cardsForColumn(candidate)) > 0 {
				m.column = candidate
				m.row = 0
				break
			}
		}
	}
	cards := m.cardsForColumn(m.column)
	if len(cards) == 0 {
		m.row = 0
	} else if m.row >= len(cards) {
		m.row = len(cards) - 1
	}
}

func (m *Model) restoreSelection(id string) {
	if id != "" {
		for columnIndex := range m.columns() {
			for rowIndex, session := range m.cardsForColumn(columnIndex) {
				if session.ID == id {
					m.column = columnIndex
					m.row = rowIndex
					return
				}
			}
		}
	}
	m.clampSelection()
}

type column struct {
	title   string
	status  agent.RuntimeStatus
	project string
	color   string
}

func (m *Model) columns() []column {
	if m.group == groupProject {
		return m.projectColumns()
	}
	// The group that wants a person comes first, in both layouts: the list is
	// read top to bottom, and the board is scanned left to right.
	columns := []column{
		{title: "NEEDS YOU", status: agent.StatusNeedsYou, color: "#F59E0B"},
		{title: "RUNNING", status: agent.StatusRunning, color: "#34D399"},
		{title: "IDLE", status: agent.StatusIdle, color: "#60A5FA"},
	}
	// Error means openagentview could not read a session's log — a fault of the
	// machine, not a stage of an agent's life — so the group only exists while
	// there is something in it. Archived sessions have no group at all:
	// archiving is asking for a session to be out of sight.
	if m.anyErrorSessions() {
		columns = append(columns, column{
			title:  "ERROR",
			status: agent.StatusError,
			color:  "#F87171",
		})
	}
	return columns
}

func (m *Model) anyErrorSessions() bool {
	query := strings.ToLower(strings.TrimSpace(m.query))
	for _, session := range m.sessions {
		if session.RuntimeStatus == agent.StatusError &&
			m.sessionVisible(session, query) {
			return true
		}
	}
	return false
}

func (m *Model) projectColumns() []column {
	type projectInfo struct {
		path      string
		updatedAt time.Time
	}
	projects := make(map[string]projectInfo)
	query := strings.ToLower(strings.TrimSpace(m.query))
	for _, session := range m.sessions {
		if session.Archived || !m.sessionVisible(session, query) {
			continue
		}
		path := filepath.Clean(session.CWD)
		current, ok := projects[path]
		if !ok || session.UpdatedAt.After(current.updatedAt) {
			projects[path] = projectInfo{path: path, updatedAt: session.UpdatedAt}
		}
	}
	sorted := make([]projectInfo, 0, len(projects))
	for _, project := range projects {
		sorted = append(sorted, project)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].updatedAt.After(sorted[j].updatedAt)
	})
	colors := []string{"#60A5FA", "#A78BFA", "#34D399", "#F59E0B", "#F472B6", "#22D3EE"}
	columns := make([]column, 0, len(sorted))
	for i, project := range sorted {
		columns = append(columns, column{
			title:   projectName(project.path),
			project: project.path,
			color:   colors[i%len(colors)],
		})
	}
	return columns
}

func (m *Model) cardsForColumn(index int) []agent.Session {
	columns := m.columns()
	if index < 0 || index >= len(columns) {
		return nil
	}
	column := columns[index]
	query := strings.ToLower(strings.TrimSpace(m.query))
	var result []agent.Session
	for _, session := range m.sessions {
		if !m.sessionVisible(session, query) {
			continue
		}
		if m.group != groupProject && session.RuntimeStatus == column.status {
			result = append(result, session)
		}
		if m.group == groupProject &&
			!session.Archived &&
			filepath.Clean(session.CWD) == column.project {
			result = append(result, session)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if ra, rb := statusRank(a.RuntimeStatus), statusRank(b.RuntimeStatus); ra != rb {
			return ra < rb
		}
		at, bt := sessionSortTime(a), sessionSortTime(b)
		if !at.Equal(bt) {
			return at.After(bt)
		}
		return a.ID < b.ID
	})
	return result
}

// statusRank is the Status board's column order replayed inside a mixed
// column, so the Projects grouping reads the same way the board does:
// what wants a person, then what is working, then what has settled.
func statusRank(status agent.RuntimeStatus) int {
	switch status {
	case agent.StatusNeedsYou:
		return 0
	case agent.StatusRunning:
		return 1
	case agent.StatusError:
		return 3
	default:
		return 2
	}
}

// sessionSortTime is the moment a card holds its place by. A session that
// is still an agent's concern sorts by when it started: its log grows on
// every poll, and cards trading places while agents work made the running
// column unreadable. A settled session sorts by when it last did anything,
// which is fixed until something happens to it — so either way, a refresh
// alone never reorders the board.
func sessionSortTime(session agent.Session) time.Time {
	switch session.RuntimeStatus {
	case agent.StatusRunning, agent.StatusNeedsYou:
		if !session.CreatedAt.IsZero() {
			return session.CreatedAt
		}
	}
	return session.UpdatedAt
}

func (m *Model) sessionVisible(session agent.Session, query string) bool {
	// A dismissed session is out of sight everywhere, search included, the
	// same way an archived one is.
	if m.dismissals != nil && m.dismissals.Dismissed(session.Agent, session.ID) {
		return false
	}
	// The agent filter is an axis of its own: it narrows what a search
	// searches rather than being overridden by one.
	if len(m.agentFilter) > 0 && !m.agentFilter[strings.ToLower(session.Agent)] {
		return false
	}
	if query != "" {
		return matches(session, query)
	}
	switch session.RuntimeStatus {
	case agent.StatusRunning, agent.StatusNeedsYou, agent.StatusError:
		return true
	}
	recency := session.RecencyAt
	if recency.IsZero() {
		recency = session.UpdatedAt
	}
	return recency.After(time.Now().Add(-recentWindow))
}

func matches(s agent.Session, query string) bool {
	query = strings.ToLower(query)
	haystack := strings.ToLower(strings.Join([]string{
		s.Agent,
		s.Title,
		s.Preview,
		s.CWD,
		s.Branch,
		s.Source,
		s.AgentNickname,
		s.AgentRole,
	}, "\n"))
	return strings.Contains(haystack, query)
}

func projectName(path string) string {
	name := filepath.Base(filepath.Clean(path))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return path
	}
	return name
}

func (m *Model) View() tea.View {
	content := m.render()
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "openagentview"
	// The mouse is captured everywhere so the board can be worked by clicking:
	// tabs, cards and the composer are all reachable without the keyboard. The
	// price is native text selection, which every terminal hands back while
	// shift (or option, on macOS) is held — the help footer says so.
	view.MouseMode = tea.MouseModeCellMotion
	// A capture holds cells, not terminal state, so the mirrored pane has no
	// cursor of its own to show. While someone is typing into it, this terminal
	// lends it one at the same place, which is the only thing on screen that
	// says where the next keystroke lands.
	if m.previewOpen && m.paneView && m.paneInput && m.paneCursorRow >= 0 {
		view.Cursor = tea.NewCursor(m.paneCursorCol, m.paneCursorRow)
	}
	return view
}

func (m *Model) render() string {
	if m.width < 1 || m.height < 1 {
		return ""
	}
	if m.previewOpen && m.previewBase != "" {
		return m.renderQuickLook(m.previewBase)
	}
	content := m.renderBase()
	if m.detail {
		return m.renderDetail()
	}
	if m.previewOpen {
		return m.renderQuickLook(content)
	}
	return content
}

func (m *Model) renderBase() string {
	// The zones are rebuilt with the frame they describe.
	m.clickZones = m.clickZones[:0]
	header := m.renderHeader()
	board := m.renderBoard()
	footer := m.renderFooter()
	if composer := m.renderComposer(); composer != "" {
		// Measured rather than derived from the height arithmetic, so the zone
		// cannot drift from where the composer actually lands.
		m.addClickZone(clickZone{
			rect: screenRect{
				x:      0,
				y:      lipgloss.Height(header) + lipgloss.Height(board),
				width:  m.width,
				height: 2,
			},
			action: clickComposer,
		})
		return lipgloss.JoinVertical(lipgloss.Left, header, board, composer, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, board, footer)
}

var (
	logoStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#E2E8F0"))
	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#64748B"))
	activeTabStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0F172A")).
			Background(lipgloss.Color("#A78BFA")).
			Padding(0, 1)
	tabStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#94A3B8")).
			Padding(0, 1)
	ruleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#334155"))
)

// dashedBorder frames the placeholder an empty column shows: dashes say
// "nothing here yet" where a solid card border would say "something".
var dashedBorder = lipgloss.Border{
	Top: "╌", Bottom: "╌", Left: "┆", Right: "┆",
	TopLeft: "╭", TopRight: "╮", BottomLeft: "╰", BottomRight: "╯",
}

// renderHeader lays the two axes out as two runs of tabs with a rule between
// them: Status and Projects say what the sessions are grouped by, Kanban and
// List say how the groups are drawn, and one of each is always lit.
func (m *Model) renderHeader() string {
	statusTab := tabStyle.Render("Status")
	projectsTab := tabStyle.Render("Projects")
	if m.group == groupProject {
		projectsTab = activeTabStyle.Render("Projects")
	} else {
		statusTab = activeTabStyle.Render("Status")
	}
	kanbanTab := tabStyle.Render("Kanban")
	listTab := tabStyle.Render("List")
	if m.layout == layoutList {
		listTab = activeTabStyle.Render("List")
	} else {
		kanbanTab = activeTabStyle.Render("Kanban")
	}

	search := mutedStyle.Render("last 24h · / search")
	if m.searching {
		search = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F8FAFC")).
			Background(lipgloss.Color("#1E293B")).
			Padding(0, 1).
			Render("Search: " + m.query + "█")
	} else if m.query != "" {
		search = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#C4B5FD")).
			Render("filter: " + m.query)
	}

	left := lipgloss.JoinHorizontal(
		lipgloss.Center,
		logoStyle.Render(" openagentview "),
		"  ",
		statusTab,
		" ",
		projectsTab,
		mutedStyle.Render("  │  "),
		kanbanTab,
		" ",
		listTab,
	)
	// The tabs' zones walk the same run of segments the line was joined from.
	x := lipgloss.Width(logoStyle.Render(" openagentview ")) + 2
	for _, tab := range []struct {
		width  int
		gap    int
		action clickAction
	}{
		{lipgloss.Width(statusTab), 1, clickGroupStatus},
		{lipgloss.Width(projectsTab), 5, clickGroupProjects},
		{lipgloss.Width(kanbanTab), 1, clickLayoutKanban},
		{lipgloss.Width(listTab), 0, clickLayoutList},
	} {
		m.addClickZone(clickZone{
			rect:   screenRect{x: x, y: 0, width: tab.width, height: 1},
			action: tab.action,
		})
		x += tab.width + tab.gap
	}
	// The chips are the newest thing on this line and so the first to give way
	// on a narrow terminal: a header that pushes the search hint off the right
	// edge costs an entry point that was there before them. Lit chips are kept
	// longest, since a filter the header cannot show is one the board seems to
	// be applying for no reason.
	chips, chipWidths := m.renderAgentChips(false)
	right := headerRight(chips, search)
	if lipgloss.Width(left)+lipgloss.Width(right)+2 > m.width {
		chips, chipWidths = m.renderAgentChips(true)
		right = headerRight(chips, search)
	}
	if lipgloss.Width(left)+lipgloss.Width(right)+2 > m.width {
		chips, chipWidths = "", nil
		// With no chips left to say so, the filter would be invisible and the
		// board would look like it was hiding sessions for no reason. The hint
		// beside the search is already a sentence about what the board is
		// showing, so the lit agents are said there instead — where "last 24h"
		// was, costing no width the header did not already spend. A search in
		// progress owns that space and is left alone.
		if len(m.agentFilter) > 0 && !m.searching {
			// The names are tried first and a count only if they do not fit,
			// and neither is worth the search entry itself: the hint keeps
			// whichever form the line still has room for, or none of them.
			forms := []string{strings.Join(m.filteredAgents(), " ")}
			if len(m.agentFilter) > 1 {
				forms = append(forms, fmt.Sprintf("%d agents", len(m.agentFilter)))
			}
			budget := m.width - lipgloss.Width(left) - 2
			for _, form := range forms {
				candidate := mutedStyle.Render(form + " · / search")
				if m.query != "" {
					candidate = mutedStyle.Render(form+" · ") + search
				}
				if lipgloss.Width(candidate) <= budget {
					search = candidate
					break
				}
			}
		}
		right = search
	}
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right)-2)
	line := left + strings.Repeat(" ", gap) + right
	// The chips' zones walk their own run of segments, the way the tabs' do.
	// A zone is only worth recording where the line is still on screen: the
	// header is cut, not wrapped, and a zone past the cut would take clicks
	// for a chip the terminal is not showing.
	x = lipgloss.Width(left) + gap
	for i, width := range chipWidths {
		if width == 0 {
			continue
		}
		if x+width <= m.width {
			m.addClickZone(clickZone{
				rect:   screenRect{x: x, y: 0, width: width, height: 1},
				action: clickAgentChip,
				column: i,
			})
		}
		x += width + 1
	}
	searchX := lipgloss.Width(left) + lipgloss.Width(right) + gap - lipgloss.Width(search)
	if !m.searching && searchX+lipgloss.Width(search) <= m.width {
		m.addClickZone(clickZone{
			rect: screenRect{
				x:      searchX,
				y:      0,
				width:  lipgloss.Width(search),
				height: 1,
			},
			action: clickSearch,
		})
	}
	// The header is one line by contract — everything below it takes its rows
	// from boardTopRow — so a narrow terminal cuts the line rather than
	// wrapping it. The search hint sits rightmost and is the first to go.
	return lipgloss.NewStyle().
		Width(m.width).
		BorderBottom(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("#334155")).
		Render(truncate(line, m.width))
}

// headerRight joins the chip run to the search hint, the two right-hand
// widgets of the header, or stands the hint alone when there are no chips.
func headerRight(chips, search string) string {
	if chips == "" {
		return search
	}
	return chips + mutedStyle.Render("  ·  ") + search
}

// renderAgentChips draws one chip per agent in the same lit/unlit shape the
// Status and Kanban tabs use, since a chip asks the same kind of question of
// the board. Unlike those tabs, any number of chips can be lit at once, and
// none lit means every agent — the board's resting state is unfiltered, so
// nothing has to be selected to see everything. It returns the joined run and
// each chip's width, which the header needs to place their click zones.
func (m *Model) renderAgentChips(litOnly bool) (string, []int) {
	names := m.agentNames()
	// A single agent has nothing to be filtered from, and a chip that can
	// only ever hide the whole board is worth less than the width it costs.
	if len(names) < 2 {
		return "", nil
	}
	chips := make([]string, 0, len(names))
	widths := make([]int, 0, len(names))
	for _, name := range names {
		lit := m.agentFilter[name]
		if litOnly && !lit {
			// Zones are placed by the chip's index among all agents, so a
			// dropped chip keeps its slot as a zero width rather than
			// renumbering the chips that stayed.
			widths = append(widths, 0)
			continue
		}
		style := tabStyle
		if lit {
			style = activeTabStyle
		}
		chip := style.Render(strings.ToUpper(name))
		chips = append(chips, chip)
		widths = append(widths, lipgloss.Width(chip))
	}
	if len(chips) == 0 {
		return "", nil
	}
	return strings.Join(chips, " "), widths
}

func (m *Model) renderBoard() string {
	columns := m.columns()
	availableHeight := max(minBoardHeight, m.height-4-m.footerHeight()-m.composerHeight())
	// Grouped by project there are no standing columns — a project only has
	// one while a session is in it — so an empty board has no columns at all
	// there, and the same empty state has to answer for both shapes.
	total := 0
	for i := range columns {
		total += len(m.cardsForColumn(i))
	}
	if len(columns) == 0 || total == 0 {
		return m.renderEmptyBoard(availableHeight)
	}
	if m.layout == layoutList {
		return m.renderList(columns, availableHeight)
	}
	if m.width < 90 {
		return m.renderCompactBoard(columns, availableHeight)
	}

	gap := 3
	maxVisible := max(1, m.width/24)
	maxVisible = min(maxVisible, len(columns))
	start := m.column - maxVisible/2
	start = max(0, min(start, len(columns)-maxVisible))
	end := start + maxVisible
	// Columns share the width evenly but stop growing past maxColumnWidth: a
	// card stretched the whole of an ultrawide terminal reads as a rule with
	// text on it rather than as a card, and its three lines are then a long
	// way apart for how little each one says.
	columnWidth := max(18, (m.width-gap*(maxVisible-1))/maxVisible)
	columnWidth = min(columnWidth, maxColumnWidth)
	// Once the columns stop growing they are centered rather than left in
	// place: an even margin on both sides reads as a deliberate measure, a
	// pile of empty space on the right reads as a bug.
	indent := max(0, (m.width-(columnWidth*maxVisible+gap*(maxVisible-1)))/2)
	rendered := make([]string, 0, maxVisible)
	for i := start; i < end; i++ {
		column := columns[i]
		originX := indent + (i-start)*(columnWidth+gap)
		rendered = append(rendered, m.renderColumn(
			i, column, columnWidth, availableHeight, originX, boardTopRow,
		))
	}
	return lipgloss.NewStyle().PaddingLeft(indent).Render(
		lipgloss.JoinHorizontal(
			lipgloss.Top,
			intersperse(rendered, strings.Repeat(" ", gap))...,
		),
	)
}

// renderEmptyBoard replaces the board when there is nothing at all to put on
// it. Empty is either "the filter matched nothing" — in which case the query
// deserves the explanation — or "no agent has run in the last day", in which
// case what earns a session a place here does.
func (m *Model) renderEmptyBoard(height int) string {
	inner := max(8, m.width-6)
	bright := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E2E8F0"))
	hint := func(key, text string) string {
		// Cut rather than wrap: a wrapped hint is a row the height budget
		// below did not account for.
		return truncate(
			ruleStyle.Render("│ ")+
				lipgloss.NewStyle().Foreground(lipgloss.Color("#94A3B8")).Render(key)+
				mutedStyle.Render("  "+text),
			inner,
		)
	}
	var lines []string
	switch query := strings.TrimSpace(m.query); {
	// Before the first discovery lands there is no answer yet, and the board
	// must not give one: "no agents running" is a finding, and a scan that
	// has not finished has found nothing at all.
	case m.lastSync.IsZero():
		lines = append(lines,
			bright.Render("Looking for sessions…"),
			"",
			mutedStyle.Render(truncate(
				"Reading tmux panes and agent logs.", inner,
			)),
		)
	case query != "":
		lines = append(lines,
			bright.Render(truncate("Nothing matches "+query+".", inner)),
			"",
			mutedStyle.Render(truncate(fmt.Sprintf(
				"Searching title, project, branch and agent across %d sessions.",
				len(m.sessions),
			), inner)),
			"",
			hint("esc", "clear the filter"),
		)
		if !m.searching {
			lines = append(lines, hint("/", "edit the query"))
		}
	// An agent filter can empty the board on its own, and saying "no agents
	// running" then would be a finding the board did not make: the sessions
	// it is hiding are its own doing. The chips say which agents are lit, but
	// a narrow header can cut them off the line, so the way out is named here
	// rather than left to the shortcut help.
	case len(m.agentFilter) > 0:
		lines = append(lines,
			bright.Render(truncate(
				"Nothing running for "+strings.Join(m.filteredAgents(), ", ")+".",
				inner,
			)),
			"",
		)
		if held := m.heldBackByAgentFilter(); held > 0 {
			lines = append(lines, mutedStyle.Render(truncate(fmt.Sprintf(
				"%d sessions are waiting behind the filter.", held,
			), inner)))
		} else {
			lines = append(lines, mutedStyle.Render(truncate(
				"No agent has run in the last day either.", inner,
			)))
		}
		lines = append(lines, "", hint("a", "show every agent"))
	default:
		lines = append(lines,
			bright.Render("No agents running."),
			"",
			mutedStyle.Render(truncate(
				"openagentview watches tmux panes and agent logs.", inner,
			)),
			mutedStyle.Render(truncate(
				"Start claude, codex, or grok anywhere and it shows up here within a second.",
				inner,
			)),
			"",
		)
		if m.canCompose() {
			lines = append(lines, hint("n", "start a task from here"))
		}
		lines = append(lines, hint("r", "rescan now"))
	}
	// Height is a budget, not a floor: lipgloss pads a short block out to it
	// but lets a tall one through, and a block that overruns pushes the
	// composer and the footer off the bottom of the terminal. The lines are
	// ordered most-important-first, so a short terminal simply keeps fewer.
	pad := 1
	if len(lines)+2 > height {
		pad = 0
	}
	if budget := height - 2*pad; len(lines) > budget {
		lines = lines[:max(1, budget)]
	}
	return lipgloss.NewStyle().
		Width(m.width).
		Height(height).
		Padding(pad, 2).
		Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

func (m *Model) renderCompactBoard(columns []column, height int) string {
	var tabs []string
	for i, column := range columns {
		count := len(m.cardsForColumn(i))
		style := tabStyle
		if i == m.column {
			style = activeTabStyle
		}
		tabs = append(tabs, style.Render(fmt.Sprintf("%s %d", column.title, count)))
	}
	tabLine := strings.Join(tabs, " ")
	if lipgloss.Width(tabLine) <= m.width {
		x := 0
		for i, tab := range tabs {
			width := lipgloss.Width(tab)
			m.addClickZone(clickZone{
				rect:   screenRect{x: x, y: boardTopRow, width: width, height: 1},
				action: clickColumnTab,
				column: i,
			})
			x += width + 1
		}
	} else {
		tabLine = mutedStyle.Render(fmt.Sprintf(
			"← %s %d/%d →",
			columns[m.column].title,
			m.column+1,
			len(columns),
		))
		// Too narrow for tabs, the line becomes a pager, and its halves page.
		width := lipgloss.Width(tabLine)
		m.addClickZone(clickZone{
			rect:   screenRect{x: 0, y: boardTopRow, width: width / 2, height: 1},
			action: clickColumnPrev,
		})
		m.addClickZone(clickZone{
			rect: screenRect{
				x:      width / 2,
				y:      boardTopRow,
				width:  width - width/2,
				height: 1,
			},
			action: clickColumnNext,
		})
	}
	column := m.renderColumn(
		m.column, columns[m.column], m.width, max(4, height-2),
		0, boardTopRow+1,
	)
	return lipgloss.JoinVertical(lipgloss.Left, tabLine, column)
}

// List is the board flattened into one column of headings and rows: every
// session in one reading order, with the line each one is sitting on rather
// than the card it fits in. It answers "what is everyone doing" on a screen the
// kanban view would need scrolling to cover.
func (m *Model) renderList(columns []column, height int) string {
	summary := m.renderListSummary(columns)
	bodyHeight := max(1, height-2)

	lines := make([]string, 0, 32)
	selectedLine := -1
	type rowRef struct{ line, column, row int }
	var rowRefs []rowRef
	for index, column := range columns {
		cards := m.cardsForColumn(index)
		if len(cards) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(column.color)).
			Render(column.title)+mutedStyle.Render(fmt.Sprintf("  %d", len(cards))))
		for row, card := range cards {
			selected := index == m.column && row == m.row
			if selected {
				selectedLine = len(lines)
			}
			rowRefs = append(rowRefs, rowRef{
				line:   len(lines),
				column: index,
				row:    row,
			})
			lines = append(lines, m.renderListRow(card, selected))
		}
	}
	if len(lines) == 0 {
		lines = []string{mutedStyle.Render("  No sessions in the last 24h")}
	}

	// Headings scroll with their rows: a heading pinned to the top would claim
	// sessions above it that are no longer on screen.
	scroll := 0
	if len(lines) > bodyHeight {
		if selectedLine >= bodyHeight {
			scroll = selectedLine - bodyHeight + 1
		}
		scroll = min(scroll, len(lines)-bodyHeight)
		lines = lines[scroll : scroll+bodyHeight]
	}
	if selectedLine >= 0 {
		// The list body sits below the summary line and the blank one after it.
		m.selectedRect = screenRect{
			x:      1,
			y:      boardTopRow + 2 + selectedLine - scroll,
			width:  max(1, m.width-2),
			height: 1,
		}
	}
	for _, ref := range rowRefs {
		if ref.line < scroll || ref.line >= scroll+bodyHeight {
			continue
		}
		m.addClickZone(clickZone{
			rect: screenRect{
				x:      1,
				y:      boardTopRow + 2 + ref.line - scroll,
				width:  max(1, m.width-2),
				height: 1,
			},
			action: clickCard,
			column: ref.column,
			row:    ref.row,
		})
	}
	for len(lines) < bodyHeight {
		lines = append(lines, "")
	}

	return lipgloss.NewStyle().
		Width(m.width).
		PaddingLeft(1).
		Render(lipgloss.JoinVertical(
			lipgloss.Left,
			append([]string{summary, ""}, lines...)...,
		))
}

// renderListSummary counts the groups that have anything in them, so the top of
// the list says how much of it there is before any of it is read.
func (m *Model) renderListSummary(columns []column) string {
	var parts []string
	for index, column := range columns {
		if count := len(m.cardsForColumn(index)); count > 0 {
			parts = append(parts, fmt.Sprintf(
				"%d %s",
				count,
				strings.ToLower(column.title),
			))
		}
	}
	if len(parts) == 0 {
		return mutedStyle.Render("no sessions")
	}
	return mutedStyle.Render(truncate(strings.Join(parts, " · "), max(1, m.width-2)))
}

// listAgeWidth holds the widest age a row is expected to carry ("47s", "365d"
// being the two shapes it takes).
const listAgeWidth = 4

// listRowNameWidth is the column the titles line up in. Descriptions only read
// as a column of their own if the titles before them end at one place.
func (m *Model) listRowNameWidth() int {
	return min(32, max(12, m.width/4))
}

// runningPulse is the running marker's animation: the asterisk swells and
// settles, the way an agent at work feels from across the room.
var runningPulse = []string{"✱", "✳", "✻", "✳"}

func (m *Model) renderListRow(session agent.Session, selected bool) string {
	marker := mutedStyle.Render("·")
	active := true
	switch session.RuntimeStatus {
	case agent.StatusNeedsYou:
		marker = lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B")).Render("✱")
	case agent.StatusRunning:
		// The pulse wears the agent's own color, so a mixed list says who is
		// working without reading a single row.
		pulseColor, ok := agentColors[strings.ToLower(session.Agent)]
		if !ok {
			pulseColor = "#34D399"
		}
		marker = lipgloss.NewStyle().
			Foreground(lipgloss.Color(pulseColor)).
			Render(runningPulse[m.animFrame%len(runningPulse)])
	case agent.StatusError:
		marker = lipgloss.NewStyle().Foreground(lipgloss.Color("#F87171")).Render("✱")
	default:
		active = false
	}

	cursor := " "
	// A settled session's title dims to the muted tone, so the rows still
	// being worked stand out of a mixed list at a glance.
	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#64748B"))
	if active {
		nameStyle = nameStyle.Foreground(lipgloss.Color("#E2E8F0"))
	}
	if selected {
		cursor = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A78BFA")).
			Render("❯")
		nameStyle = nameStyle.Bold(true).Foreground(lipgloss.Color("#F8FAFC"))
	}

	nameWidth := m.listRowNameWidth()
	name := truncate(session.Title, nameWidth)
	name += strings.Repeat(" ", max(0, nameWidth-lipgloss.Width(name)))

	// The age is right-aligned in a fixed column: read down the list, it is a
	// ranking of how stale each session is, which only holds if the numbers end
	// at the same place.
	age := truncate(shortAge(session.UpdatedAt), listAgeWidth)
	age = strings.Repeat(" ", max(0, listAgeWidth-lipgloss.Width(age))) + age

	line := cursor + " " + marker + " " + nameStyle.Render(name)
	// What is left after the row's own indent, the two-space gutter before the
	// description, the space before the age, and the column the list is padded
	// by — plus one more so the age never sits against the terminal's edge.
	descriptionWidth := m.width - lipgloss.Width(line) - 2 - listAgeWidth - 3
	if descriptionWidth < 8 {
		return line
	}
	description := truncate(
		listDescription(session, m.group != groupProject),
		descriptionWidth,
	)
	padding := descriptionWidth - lipgloss.Width(description)
	return line + "  " + mutedStyle.Render(description) +
		strings.Repeat(" ", padding+1) + mutedStyle.Render(age)
}

// listDescription is what the session is about, said in one line. The first
// prompt is the best answer available; where it is also the title — which is
// what a session without a summary falls back to — repeating it says nothing,
// so the session's whereabouts are shown instead. Grouped by project the
// heading above the row has already named the project, so the whereabouts
// leave it out.
func listDescription(session agent.Session, withProject bool) string {
	preview := strings.TrimSpace(strings.ReplaceAll(session.Preview, "\n", " "))
	if preview != "" && preview != strings.TrimSpace(session.Title) {
		return preview
	}
	location := session.Agent
	if withProject {
		location += " · " + projectName(session.CWD)
	}
	if session.TmuxTarget != "" {
		location += " · tmux " + session.TmuxTarget
	} else if session.Branch != "" {
		location += " · " + session.Branch
	}
	return location
}

// shortAge is relativeTime without the "ago": in a column of ages every value
// means the same thing, and the word is width the descriptions can use.
func shortAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// renderColumn draws one column of the board. originX and originY are the
// terminal cell its top-left corner lands on, which is how the selected card's
// on-screen rectangle is known without re-deriving the board's layout.
func (m *Model) renderColumn(
	index int,
	column column,
	width, height, originX, originY int,
) string {
	cards := m.cardsForColumn(index)
	// The header is the group's name, its count, and a rule out to the
	// column's edge — the rule keeps an empty column visibly a column. A
	// count of zero dims the name: the gap is the signal, not the label.
	headerWidth := max(1, width-2)
	titleColor := column.color
	if len(cards) == 0 {
		titleColor = "#475569"
	}
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(titleColor)).
		Render(column.title) +
		mutedStyle.Render(fmt.Sprintf("  %d", len(cards)))
	if ruleWidth := headerWidth - lipgloss.Width(header) - 1; ruleWidth > 0 {
		header += " " + ruleStyle.Render(strings.Repeat("─", ruleWidth))
	}
	header = truncate(header, headerWidth)

	// A card occupies cardHeight rows and the one below it is cardStride rows
	// further down: the row between them is what keeps a run of cards reading
	// as separate cards rather than as one ruled table. A column too short
	// for even that gives the row up rather than clip a card.
	roomy := height-1 >= cardStride
	stride := cardStride
	if !roomy {
		stride = compactCardHeight
	}
	// n cards laid on this stride occupy n*stride rows less the gap the last
	// one does not need, under a header row: n*stride - (stride-cardHeight)
	// <= height-1. Counting the trailing gap as spent would drop a card that
	// fits exactly, and make the column scroll for nothing.
	capacity := max(1, (height-1+stride-cardHeight)/stride)
	start := 0
	if index == m.column && m.row >= capacity {
		start = m.row - capacity + 1
	}
	end := min(len(cards), start+capacity)
	var renderedCards []string
	for i := start; i < end; i++ {
		selected := index == m.column && i == m.row
		rect := screenRect{
			x:      originX + 1,
			y:      originY + 1 + (i-start)*stride,
			width:  width - 2,
			height: cardHeight,
		}
		if selected {
			m.selectedRect = rect
		}
		m.addClickZone(clickZone{
			rect:   rect,
			action: clickCard,
			column: index,
			row:    i,
		})
		if len(renderedCards) > 0 && roomy {
			renderedCards = append(renderedCards, "")
		}
		renderedCards = append(
			renderedCards,
			m.renderCard(cards[i], width-2, selected, column.color),
		)
	}
	if len(renderedCards) == 0 {
		renderedCards = append(
			renderedCards,
			m.emptyColumnBox(column.status, width-2),
		)
	}
	body := lipgloss.JoinVertical(lipgloss.Left, renderedCards...)
	return lipgloss.NewStyle().
		Width(width).
		Height(height).
		Padding(0, 1).
		Render(lipgloss.JoinVertical(lipgloss.Left, header, body))
}

// agentColors is each agent's own theme hue: claude in Anthropic's coral,
// codex in blue, grok in gold. The badge is a label to recognize, not a
// signal to react to — attention is the amber dot's job — but the label keeps
// its color even on a settled card: which agent it was is the one thing a
// card never stops saying.
var agentColors = map[string]string{
	"claude": "#D97757",
	"codex":  "#4E9EFF",
	"grok":   "#E0A84E",
}

// agentBadge is the agent's name at the card's top-left, in the agent's own
// color so two otherwise identical cards tell apart at a glance. Color is the
// whole badge: neither a filled block nor a drawn outline earns the width and
// weight it costs on the card's least important line.
func agentBadge(s agent.Session) string {
	color, ok := agentColors[strings.ToLower(s.Agent)]
	if !ok {
		color = "#475569"
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color(color)).
		Render(strings.ToLower(s.Agent))
}

func (m *Model) renderCard(
	s agent.Session,
	width int,
	selected bool,
	color string,
) string {
	inner := max(8, width-cardChrome)

	// A session nobody is waiting on and nothing is happening in gets no
	// color and no brightness: color on this board means an agent is at work
	// or wants a person, and a wall of settled sessions wearing their agents'
	// colors — which is what the project grouping is — says neither. Dimming
	// the whole card is also what makes the few live ones findable.
	live := s.RuntimeStatus == agent.StatusRunning ||
		s.RuntimeStatus == agent.StatusNeedsYou ||
		s.RuntimeStatus == agent.StatusError
	projectColor, titleColor := "#CBD5E1", "#F8FAFC"
	if !live {
		projectColor, titleColor = "#64748B", "#94A3B8"
	}

	// Selection is drawn on the border, and on a settled card it is drawn in
	// gray: a board of nothing but idle sessions would otherwise always have
	// one card wearing its group's color, which is the very thing the color
	// is supposed to mean.
	borderColor := "#334155"
	if selected {
		borderColor = color
		if !live {
			borderColor = "#94A3B8"
		}
	}

	// The top line is who and where: the agent's badge, the project, and —
	// right-aligned, in the card's faintest tone — the branch, or the tmux
	// pane when there is no branch to name. Grouped by project the column
	// header has already said which project this is, so the card drops it
	// and spends the width on the branch instead.
	badge := agentBadge(s)
	dot := ""
	if s.RuntimeStatus == agent.StatusNeedsYou {
		dot = " " + lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B")).Render("●")
	}
	meta := badge + dot
	if m.group != groupProject {
		project := truncate(
			filepath.Base(s.CWD),
			max(1, inner-lipgloss.Width(badge)-1-lipgloss.Width(dot)),
		)
		meta = badge + " " +
			lipgloss.NewStyle().Foreground(lipgloss.Color(projectColor)).Render(project) + dot
	}
	right := s.Branch
	if right == "" && s.TmuxTarget != "" {
		right = "tmux " + s.TmuxTarget
	}
	if avail := inner - lipgloss.Width(meta) - 2; right != "" && avail >= 4 {
		right = truncate(right, avail)
		gap := inner - lipgloss.Width(meta) - lipgloss.Width(right)
		meta += strings.Repeat(" ", gap) +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#475569")).Render(right)
	}

	titleRows := wrapTitleRows(s.Title, inner)

	// The bottom line is what the session is doing or asking, amber when it
	// is a question waiting on a person. A session whose preview only repeats
	// its title has nothing to add, so its age speaks instead; a settled
	// session carries its age alongside.
	detail := strings.TrimSpace(strings.ReplaceAll(s.Preview, "\n", " "))
	if detail == strings.TrimSpace(s.Title) {
		detail = ""
	}
	if detail == "" {
		detail = relativeTime(s.UpdatedAt)
	} else if s.RuntimeStatus == agent.StatusIdle {
		detail += " · " + shortAge(s.UpdatedAt)
	}
	detailStyle := mutedStyle
	if !live {
		detailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#475569"))
	}
	if s.RuntimeStatus == agent.StatusNeedsYou {
		detailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B"))
	}
	return lipgloss.NewStyle().
		Width(width).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 1).
		Render(
			// The three lines stay tight against each other. A terminal's
			// only unit of leading is a whole blank row, and a row between
			// them is more air than the card has content to justify — it
			// reads as three loose lines rather than as one card.
			meta + "\n" +
				// The title is the card: always bold, always the brightest
				// line it has. Selection is the border's job. It always gets
				// two rows — a one-line title leaves the second blank rather
				// than shrinking the card.
				lipgloss.NewStyle().
					Bold(true).
					Foreground(lipgloss.Color(titleColor)).
					Render(titleRows[0]) +
				"\n" +
				lipgloss.NewStyle().
					Bold(true).
					Foreground(lipgloss.Color(titleColor)).
					Render(titleRows[1]) +
				"\n" + detailStyle.Render(truncate(detail, inner)),
		)
}

// wrapTitleRows lays a title over exactly two rows of the given width: broken
// at a word boundary when it can be, hard-truncated at the end of the second
// row when even two rows cannot hold it.
func wrapTitleRows(title string, width int) [2]string {
	title = strings.Join(strings.Fields(title), " ")
	if lipgloss.Width(title) <= width {
		return [2]string{title, ""}
	}
	words := strings.Fields(title)
	first := ""
	for i, word := range words {
		candidate := word
		if first != "" {
			candidate = first + " " + word
		}
		if lipgloss.Width(candidate) > width && first != "" {
			return [2]string{
				truncate(first, width),
				truncate(strings.Join(words[i:], " "), width),
			}
		}
		first = candidate
	}
	return [2]string{truncate(title, width), ""}
}

// emptyColumnBox is what an empty column holds instead of cards: a dashed
// frame with a line on why it is empty, so a bare column reads as a fact
// about the fleet rather than a rendering gap. Nothing in it is colored —
// color on this board means a session wants something, and an empty column is
// the one place with nothing to want.
func (m *Model) emptyColumnBox(status agent.RuntimeStatus, width int) string {
	var lead, detail string
	switch status {
	case agent.StatusNeedsYou:
		lead = "nothing waiting on you"
		detail = "agents will land here when they ask"
	case agent.StatusRunning:
		lead = "no agents running"
		detail = "sessions show up here within a second"
		if m.canCompose() {
			detail = "press n to start a task"
		}
	case agent.StatusIdle:
		lead = "no idle sessions"
		detail = "finished work moves out after 24h"
	default:
		lead = "no sessions"
	}
	inner := max(8, width-cardChrome)
	body := mutedStyle.Render(truncate(lead, inner))
	if detail != "" {
		body += "\n" + lipgloss.NewStyle().
			Foreground(lipgloss.Color("#475569")).
			Render(truncate(detail, inner))
	}
	return lipgloss.NewStyle().
		Width(width).
		Border(dashedBorder).
		BorderForeground(lipgloss.Color("#334155")).
		Padding(0, 1).
		Render(body)
}

// renderComposer draws the standing input at the bottom of the board: the
// place a new task is described without leaving what is already running. Idle
// it says what it is for; focused it carries the text being typed, cut from
// the left so the end being typed at stays in view.
func (m *Model) renderComposer() string {
	if !m.canCompose() {
		return ""
	}
	// The agent the task will go to leads the line: which agent answers is
	// the first fact about the session being described, and at the far right
	// of a wide terminal it was read last or not at all. Brackets make it a
	// selector rather than stray text; ⇥ says tab switches it.
	agentTag := m.launchers[m.composeAgent].Agent
	if len(m.launchers) > 1 {
		agentTag += " ⇥"
	}
	tagStyle := mutedStyle
	if m.composing {
		tagStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#C4B5FD"))
	}
	// The directory shows only once an @ has pointed the task somewhere
	// other than the board's own — that pick would otherwise be invisible,
	// since accepting it strips the token from the text. It wears the @ it
	// was picked with; a second bracketed tag read as a second agent.
	tag := tagStyle.Render("["+agentTag+"]") + " "
	dir := filepath.Clean(m.currentComposeDir())
	if dir != filepath.Clean(m.workdir) {
		tag += tagStyle.Render("@"+projectName(dir)) + " "
	}

	prompt := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#A78BFA")).
		Render("❯ ")
	available := max(1, m.width-2-lipgloss.Width(tag)-2)
	var line string
	if m.composing {
		text := tailCells(m.composeText, available-1)
		line = tag + prompt + lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F8FAFC")).
			Render(text) + "█"
	} else {
		line = tag + prompt + mutedStyle.Render(truncate(
			"describe a task for a new session · press n",
			available,
		))
	}
	content := truncate(line, m.width)
	// The completion menu stacks above the input, freshest match on top,
	// so the input line never moves out from under the cursor.
	if menu := m.composeMenuRows(); len(menu) > 0 {
		content = strings.Join(append(menu, content), "\n")
	}
	return lipgloss.NewStyle().
		Width(m.width).
		BorderTop(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("#334155")).
		Render(content)
}

// composeMenuRows renders the @ completion menu, one directory per row: the
// project's name leading, its path after in the muted tone, the highlighted
// row marked the way the prompt is.
func (m *Model) composeMenuRows() []string {
	if !m.composeMentionActive() {
		return nil
	}
	entries := m.composeMenuEntries()
	if len(entries) == 0 {
		// The menu owns enter for as long as the token stands, so it cannot
		// silently vanish on a typo — it says why nothing will happen.
		return []string{mutedStyle.Render(truncate(
			"  no matching directory · esc keeps the text as typed",
			max(1, m.width),
		))}
	}
	sel := min(m.composeMenuSel, len(entries)-1)
	rows := make([]string, 0, len(entries))
	for i, entry := range entries {
		marker := "  "
		nameStyle := mutedStyle
		if i == sel {
			marker = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A78BFA")).
				Render("❯ ")
			nameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8FAFC"))
		}
		row := marker + nameStyle.Render(projectName(entry)) + "  " +
			mutedStyle.Render(abbreviateHome(entry))
		rows = append(rows, truncate(row, m.width))
	}
	return rows
}

// composerHeight is the input line and the rule above it — plus the
// completion menu while one is up — or nothing when there is no agent to
// launch.
func (m *Model) composerHeight() int {
	if !m.canCompose() {
		return 0
	}
	return 2 + len(m.composeMenuRows())
}

// tailCells keeps the end of a string that fits in width display cells. The
// composer's cursor sits at the end of the text, so when the text outgrows the
// line it is the start that can go.
func tailCells(text string, width int) string {
	if lipgloss.Width(text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 {
		runes = runes[1:]
		if lipgloss.Width(string(runes)) < width {
			break
		}
	}
	return "…" + string(runes)
}

func (m *Model) renderFooter() string {
	if m.helpOpen {
		return m.renderShortcutHelp()
	}
	help := "enter quick look · ctrl+enter open in tab · tab group · v layout · ? shortcuts"
	if m.composing {
		help = "enter start the session · tab switch agent" +
			" · @ pick a project or path · esc put it down"
		if m.composeMentionActive() {
			help = "↑↓ choose · enter pick · tab complete · esc keep the text"
		}
	}
	right := ""
	if m.loading {
		right = "refreshing…"
	} else if m.status != "" {
		right = m.status
	} else if !m.lastSync.IsZero() {
		right = "synced " + relativeTime(m.lastSync)
	}
	available := max(0, m.width-lipgloss.Width(right)-2)
	help = truncate(help, available)
	gap := max(1, m.width-lipgloss.Width(help)-lipgloss.Width(right))
	return mutedStyle.Width(m.width).Render(help + strings.Repeat(" ", gap) + right)
}

func (m *Model) footerHeight() int {
	if !m.helpOpen {
		return 1
	}
	// The rule on top of the help block takes a line of its own.
	return len(m.shortcutHelpLines()) + 1
}

// shortcutHelpLines is deliberately one short line of the keys worth
// memorizing, split in two only when the terminal cannot hold it whole.
// Everything contextual teaches itself where it applies — the standing
// footer names the views, the composer and Quick Look each carry their own
// hints — and a wall of every binding reads as none of them.
func (m *Model) shortcutHelpLines() []string {
	const wide = "↑↓←→ move · enter quick look · ctrl+enter open in a tab" +
		" · n new task · / search · 1-9 filter agent · a all agents" +
		" · ctrl+x ×2 dismiss · q / ctrl+q quit"
	// One padding column on the left, and one spare on the right so the
	// line never kisses the terminal's edge.
	if lipgloss.Width(wide) <= m.width-2 {
		return []string{wide}
	}
	return []string{
		"↑↓←→ move · enter quick look · ctrl+enter open in a tab",
		"n new task · / search · 1-9 filter agent · a all agents" +
			" · ctrl+x ×2 dismiss · q / ctrl+q quit",
	}
}

func (m *Model) renderShortcutHelp() string {
	lines := m.shortcutHelpLines()
	// Each line is truncated to the width rather than wrapped, the way every
	// other footer line is — footerHeight promises exactly len(lines)+1 rows,
	// and a wrap below 55 columns would silently break that promise.
	for i, line := range lines {
		lines[i] = truncate(line, max(1, m.width-2))
	}
	return lipgloss.NewStyle().
		Width(m.width).
		BorderTop(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("#475569")).
		Foreground(lipgloss.Color("#94A3B8")).
		PaddingLeft(1).
		Render(strings.Join(lines, "\n"))
}

func (m *Model) renderQuickLook(base string) string {
	selected := m.previewedSession()
	if selected == nil {
		return base
	}
	if m.previewAnimating {
		progress := float64(time.Since(m.previewOpenedAt)) /
			float64(previewOpenDuration)
		if progress < 1 {
			return m.renderQuickLookOpening(base, *selected, progress)
		}
		m.previewAnimating = false
	}
	width, height := m.quickLookDimensions()
	contentWidth := m.quickLookContentWidth()
	bodyHeight := m.quickLookBodyHeight()

	bodyLines := m.previewLayout
	moreLabel, newerLabel := "↑ more conversation", "↓ newer conversation"
	if m.paneView {
		bodyLines = m.paneBodyLines(contentWidth)
		moreLabel, newerLabel = "↑ more of the pane", "↓ rest of the pane"
	} else if m.previewLayoutWidth != contentWidth {
		bodyLines = []string{"Reflowing conversation…"}
	} else if len(bodyLines) == 0 {
		bodyLines = []string{"Loading conversation…"}
	}
	start := max(0, len(bodyLines)-bodyHeight-m.previewScrollBack)
	end := min(len(bodyLines), start+bodyHeight)
	if start == 0 && len(bodyLines) > bodyHeight {
		end = bodyHeight
	}
	m.paneCursorRow = m.cursorRow(start, end)
	visibleLines := append([]string(nil), bodyLines[start:end]...)
	if start > 0 && len(visibleLines) > 0 {
		visibleLines[0] = mutedStyle.Render(moreLabel)
	}
	if end < len(bodyLines) && len(visibleLines) > 0 {
		visibleLines[len(visibleLines)-1] = mutedStyle.Render(newerLabel)
	}
	// Padded by hand: the lines are already wrapped to contentWidth, and asking
	// Lip Gloss to size the block would re-measure every grapheme in it on
	// every scroll step.
	for len(visibleLines) < bodyHeight {
		visibleLines = append(visibleLines, "")
	}
	body := strings.Join(visibleLines, "\n")

	title := truncate(selected.Title, contentWidth-16)
	subtitle := projectName(selected.CWD) + " · " + string(m.previewDisplayStatus(*selected))
	if m.paneView {
		subtitle = projectName(selected.CWD) + " · live pane " + selected.TmuxTarget
		if history := m.paneHistoryLabel(); history != "" {
			subtitle += " · " + history
		}
		if m.paneWidth > contentWidth {
			// The pane is a rendered screen, not text: it cannot be rewrapped to
			// fit, so say what is missing instead of pretending it fits.
			subtitle += fmt.Sprintf(
				" · %d cols cut off",
				m.paneWidth-contentWidth,
			)
		}
	} else if m.previewTranscriptReturnsToPane {
		subtitle = projectName(selected.CWD) +
			" · session transcript · live pane continues below"
	}
	if !m.paneView && m.previewTranscriptLoadingMore {
		subtitle += " · loading older history…"
	}
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#F8FAFC")).
		Render("Quick Look  " + title)
	meta := mutedStyle.Render(truncate(subtitle, contentWidth))
	rule := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#475569")).
		Render(strings.Repeat("─", contentWidth))
	footer := m.renderQuickLookFooter(contentWidth)
	boxContent := lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		meta,
		rule,
		body,
		rule,
		footer,
	)
	if m.paneView && !m.paneFloats() {
		// A pane this wide is shown at its own size or not at all: a rendered
		// screen cannot be rewrapped, so every column spent on a border or a
		// margin is a column of the agent's screen that gets cut instead. The
		// mirror takes the whole terminal, which is the one width left that
		// stands a chance of matching the pane's.
		m.paneCursorCol = m.paneCursor.CursorX
		// A fullscreen mirror leaves no board around it to click.
		m.quickLookRect = screenRect{x: 0, y: 0, width: m.width, height: m.height}
		return lipgloss.NewStyle().
			Width(m.width).
			Height(m.height).
			Background(lipgloss.Color("#0F172A")).
			Foreground(lipgloss.Color("#CBD5E1")).
			Render(boxContent)
	}

	padding := 2
	if m.paneView {
		padding = m.paneFrame().padding
	}
	box := lipgloss.NewStyle().
		Width(width).
		Height(height).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#A78BFA")).
		Background(lipgloss.Color("#0F172A")).
		Foreground(lipgloss.Color("#CBD5E1")).
		Padding(0, padding).
		Render(boxContent)

	// Measuring the box means measuring every grapheme in it, so it is done
	// once rather than once per comparison below.
	boxWidth, boxHeight := lipgloss.Width(box), lipgloss.Height(box)
	x := max(0, (m.width-boxWidth)/2)
	y := max(0, (m.height-boxHeight)/2)
	m.quickLookRect = screenRect{x: x, y: y, width: boxWidth, height: boxHeight}
	// The body rows were placed against the box; the window they sit in is
	// somewhere else on the screen, and the borrowed cursor has to follow it.
	if m.paneCursorRow >= 0 {
		m.paneCursorRow += y + 1
		m.paneCursorCol = x + 1 + padding + m.paneCursor.CursorX
	}
	if m.previewBase == base {
		// The window changes size when the mirror learns how wide its pane is,
		// and once more if it is toggled back to the transcript. The backdrop is
		// rebuilt for the new size rather than abandoned, because the frames
		// after it are all the same size again — and those are the ones being
		// drawn several times a second.
		if m.previewBackdrop.x != x ||
			m.previewBackdrop.y != y ||
			m.previewBackdrop.boxWidth != boxWidth ||
			m.previewBackdrop.boxHeight != boxHeight {
			m.previewBackdrop = newPreviewBackdrop(
				base,
				x,
				y,
				boxWidth,
				boxHeight,
				m.width,
				m.height,
			)
		}
		return m.previewBackdrop.compose(box)
	}
	return overlayANSI(base, box, x, y, m.width, m.height)
}

// renderQuickLookOpening draws one frame of the zoom: an empty overlay frame
// interpolated between the selected card and where the overlay will land, on
// the macOS Quick Look ease — fast out of the card, settling into place. The
// content is withheld until the window stops moving; text sliding diagonally
// across a terminal reads as glitch, not motion.
func (m *Model) renderQuickLookOpening(
	base string,
	selected agent.Session,
	progress float64,
) string {
	boxWidth, boxHeight := m.quickLookDimensions()
	to := screenRect{
		x:      max(0, (m.width-boxWidth)/2),
		y:      max(0, (m.height-boxHeight)/2),
		width:  boxWidth,
		height: boxHeight,
	}
	from := m.previewAnimFrom
	if from.width <= 0 || from.height <= 0 {
		// Nothing recorded to grow out of — a board that has never rendered a
		// selection — so grow out of the overlay's own centre.
		from = screenRect{
			x:      to.x + to.width/2,
			y:      to.y + to.height/2,
			width:  4,
			height: 2,
		}
	}
	frame := zoomFrame(zoomSource(from, m.height), to, progress)
	title := ""
	// Two border and two padding columns stand between the frame's width and
	// its content.
	if innerWidth := frame.width - 4; innerWidth >= 24 {
		title = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F8FAFC")).
			Render(truncate("Quick Look  "+selected.Title, innerWidth))
	}
	// Lip Gloss counts the border inside Width and Height, so the frame's own
	// dimensions go in whole.
	box := lipgloss.NewStyle().
		Width(frame.width).
		Height(frame.height).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#A78BFA")).
		Background(lipgloss.Color("#0F172A")).
		Padding(0, 1).
		Render(title)
	// The borrowed cursor stays hidden until the window has landed.
	m.paneCursorRow = -1
	// A click while the window is still growing is judged against where the
	// window is, not where it will land.
	m.quickLookRect = frame
	return overlayANSI(base, box, frame.x, frame.y, m.width, m.height)
}

// zoomSource makes a drawable first frame out of the selected card's
// rectangle. A List row is one cell tall, and a bordered frame cannot be:
// its smallest honest form is three rows hugging the row it grows out of,
// with the row at the frame's centre rather than under its border.
func zoomSource(from screenRect, screenHeight int) screenRect {
	if from.height >= 3 {
		return from
	}
	from.y = max(0, min(from.y-1, screenHeight-3))
	from.height = 3
	return from
}

// zoomFrame is where the zoom's window sits at a point in its run: exactly on
// the card at 0, exactly on the overlay at 1, and easing out in between.
func zoomFrame(from, to screenRect, progress float64) screenRect {
	eased := 1 - math.Pow(1-max(0, min(1, progress)), 3)
	return screenRect{
		x:      lerp(from.x, to.x, eased),
		y:      lerp(from.y, to.y, eased),
		width:  max(4, lerp(from.width, to.width, eased)),
		height: max(2, lerp(from.height, to.height, eased)),
	}
}

func lerp(from, to int, t float64) int {
	return from + int(math.Round(float64(to-from)*t))
}

// renderQuickLookFooter puts the live activity ahead of the key hints, and
// gives up the hints entirely rather than wrapping a narrow overlay.
func (m *Model) renderQuickLookFooter(contentWidth int) string {
	hints := "↑↓ scroll · pgup/pgdn · g/G · enter open · space/esc close"
	selected := m.previewedSession()
	history := m.paneHistoryLabel()
	switch {
	case m.paneInput && selected != nil:
		typing := "● typing into " + selected.TmuxTarget +
			" · wheel up reads history" +
			" · esc stops typing · ctrl+c interrupts agent"
		if history != "" {
			typing = history + " · wheel up continues in transcript" +
				" · typing into " + selected.TmuxTarget +
				" · esc stops typing · ctrl+c interrupts agent"
		}
		return lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FBBF24")).
			Render(truncate(typing, contentWidth))
	case m.paneView:
		hints = "↑ past top opens transcript · t session transcript · i type · enter open in tab · esc close"
		if history != "" {
			hints = history + " · " + hints
		}
	case m.previewTranscriptReturnsToPane:
		hints = "↓ past bottom returns live pane · ↑↓ transcript · t live pane · esc close"
	case selected != nil && m.canMirrorPane(*selected):
		hints = "t live pane · ↑↓ scroll · enter open in tab · esc close"
	}
	var note string
	if selected != nil && !m.paneView && !m.canMirrorPane(*selected) &&
		m.previewLive() {
		// A live session without a pane is the one case where the missing
		// ability to type needs explaining: a finished transcript reads as a
		// record on its own, but a running one looks like it should answer.
		// The note is kept apart from the key hints because it must outlive
		// them: a live session always has an activity line, and behind one the
		// hints are dropped whole — keys can be learned once, the explanation
		// cannot.
		note = "view only (not in tmux)"
	}
	activity := m.previewActivityLine()
	if activity == "" {
		if note != "" {
			hints = note + " · " + hints
		}
		return mutedStyle.Render(truncate(hints, contentWidth))
	}
	activity = truncate(activity, contentWidth)
	rendered := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#34D399")).
		Render(activity)
	remaining := contentWidth - utf8.RuneCountInString(activity) - 3
	if note != "" {
		if remaining < utf8.RuneCountInString(note) {
			return rendered
		}
		rendered += mutedStyle.Render(" · " + note)
		remaining -= utf8.RuneCountInString(note) + 3
	}
	if remaining < utf8.RuneCountInString(hints) {
		return rendered
	}
	return rendered + mutedStyle.Render(" · "+hints)
}

// paneHistoryLabel says where an honest live mirror ends. Full-screen TUIs
// redraw an alternate screen that tmux never adds to scrollback; ordinary
// panes can still end at the oldest row their window was configured to keep.
// The footer explains that scrolling past this honest edge continues through
// the transcript, without pretending the two sources are one terminal record.
func (m *Model) paneHistoryLabel() string {
	if !m.paneView || !m.paneLoaded || m.paneErr != nil {
		return ""
	}
	switch {
	case m.paneCursor.AlternateScreen:
		return "current screen only"
	case m.paneCursor.HistorySize == 0:
		return "no older pane history"
	case m.previewScrollBack > 0 &&
		m.previewScrollBack == m.previewMaxScrollBack():
		return "oldest retained pane history"
	default:
		return ""
	}
}

func (m *Model) setPreviewBase(base string) {
	m.previewBase = base
	boxWidth, boxHeight := m.quickLookDimensions()
	x := max(0, (m.width-boxWidth)/2)
	y := max(0, (m.height-boxHeight)/2)
	m.previewBackdrop = newPreviewBackdrop(
		base,
		x,
		y,
		boxWidth,
		boxHeight,
		m.width,
		m.height,
	)
}

func newPreviewBackdrop(
	base string,
	x, y, boxWidth, boxHeight, width, height int,
) previewBackdrop {
	baseLines := strings.Split(base, "\n")
	if height > 0 && len(baseLines) > height {
		baseLines = baseLines[:height]
	}
	requiredLines := y + boxHeight
	if height > 0 {
		requiredLines = min(requiredLines, height)
	}
	if len(baseLines) < requiredLines {
		baseLines = append(baseLines, make([]string, requiredLines-len(baseLines))...)
	}
	backdrop := previewBackdrop{
		baseLines: baseLines,
		left:      make([]string, boxHeight),
		right:     make([]string, boxHeight),
		x:         x,
		y:         y,
		boxWidth:  boxWidth,
		boxHeight: boxHeight,
		baseBytes: len(base),
	}
	for overlayRow := range boxHeight {
		row := y + overlayRow
		baseLine := ""
		if row < len(baseLines) {
			baseLine = baseLines[row]
		}
		left := ansi.Cut(baseLine, 0, x)
		backdrop.left[overlayRow] =
			left + strings.Repeat(" ", max(0, x-lipgloss.Width(left)))
		backdrop.right[overlayRow] =
			ansi.Cut(baseLine, x+boxWidth, width)
	}
	return backdrop
}

func (b previewBackdrop) compose(overlay string) string {
	overlayLines := strings.Split(overlay, "\n")
	var result strings.Builder
	result.Grow(len(overlay) + b.baseBytes)
	for row, baseLine := range b.baseLines {
		if row > 0 {
			result.WriteByte('\n')
		}
		overlayRow := row - b.y
		if overlayRow < 0 ||
			overlayRow >= len(overlayLines) ||
			overlayRow >= b.boxHeight {
			result.WriteString(baseLine)
			continue
		}
		result.WriteString(b.left[overlayRow])
		result.WriteString(overlayLines[overlayRow])
		result.WriteString(b.right[overlayRow])
	}
	return result.String()
}

// overlayANSI replaces only the rows covered by overlay. Unlike Lip Gloss's
// compositor, it doesn't allocate a terminal-sized cell buffer on every
// scroll event.
func overlayANSI(base, overlay string, x, y, width, height int) string {
	baseLines := strings.Split(base, "\n")
	overlayLines := strings.Split(overlay, "\n")
	overlayWidth := lipgloss.Width(overlay)
	lineCount := max(len(baseLines), min(height, y+len(overlayLines)))
	if height > 0 {
		lineCount = min(lineCount, height)
	}

	lines := make([]string, lineCount)
	for row := range lineCount {
		baseLine := ""
		if row < len(baseLines) {
			baseLine = baseLines[row]
		}
		overlayRow := row - y
		if overlayRow < 0 || overlayRow >= len(overlayLines) {
			lines[row] = baseLine
			continue
		}

		left := ansi.Cut(baseLine, 0, x)
		left += strings.Repeat(" ", max(0, x-lipgloss.Width(left)))
		middle := ansi.Cut(overlayLines[overlayRow], 0, overlayWidth)
		middle += strings.Repeat(" ", max(0, overlayWidth-lipgloss.Width(middle)))
		right := ansi.Cut(baseLine, x+overlayWidth, width)
		lines[row] = left + middle + right
	}
	return strings.Join(lines, "\n")
}

func (m *Model) quickLookBodyHeight() int {
	_, height := m.quickLookDimensions()
	if m.paneView && !m.paneFloats() {
		// Header, subtitle, two rules and the footer; no border to pay for.
		return max(4, height-5)
	}
	return max(4, height-8)
}

// paneWindowFrame is what a floating mirror spends to look like a window:
// chrome is the total width of the frame, padding is what sits between the
// border and the pane on each side, and margin is the board left showing
// around it.
type paneWindowFrame struct {
	chrome  int
	padding int
	margin  int
}

// paneWindowVMargin is the board left showing above and below any floating
// mirror: enough to clear the two-row header and the footer, which is what
// makes the window read as sitting on the board rather than replacing it.
const paneWindowVMargin = 3

func (f paneWindowFrame) floats() bool { return f.chrome > 0 }

// paneWindowChrome is the roomy frame: a border column and two padding columns
// on each side.
const paneWindowChrome = 6

// roomyPaneFrame is the frame a mirror gets whenever the pane leaves room for
// it — and therefore also the frame a pane the product sizes itself should be
// sized for.
var roomyPaneFrame = paneWindowFrame{
	chrome:  paneWindowChrome,
	padding: 2,
	margin:  2,
}

// paneFrame picks how much frame the mirror can afford. A captured screen
// cannot be rewrapped, so a frame is always paid for out of the agent's own
// columns — but a pane that fills the terminal is the common case rather than
// the exception (agents are usually run in a window the size of this one), and
// a mirror indistinguishable from the terminal around it is worth two columns
// of the screen it shows. Hence a second, tighter frame instead of giving up:
// a border, no padding, and the two columns it costs reported in the subtitle.
func (m *Model) paneFrame() paneWindowFrame {
	roomy := roomyPaneFrame
	if !m.paneView {
		return roomy
	}
	if m.paneScreenWidth+roomy.chrome+2*roomy.margin <= m.width {
		return roomy
	}
	// Below this a frame stops being decoration and starts being most of the
	// screen, so the mirror takes the terminal whole.
	if m.width >= 40 && m.height >= 14 {
		return paneWindowFrame{chrome: 2, padding: 0, margin: 1}
	}
	return paneWindowFrame{}
}

// paneFloats reports whether the mirror is a window rather than the whole
// terminal.
func (m *Model) paneFloats() bool {
	return m.paneView && m.paneFrame().floats()
}

func (m *Model) quickLookDimensions() (int, int) {
	if m.paneView {
		frame := m.paneFrame()
		if !frame.floats() {
			return m.width, m.height
		}
		available := m.width - 2*frame.margin
		// The window is sized to the pane, not to a fraction of the terminal:
		// a column past the pane's own width is padding inside the frame, and a
		// column short of it is a column of the agent's screen cut off.
		width := max(m.paneScreenWidth+frame.chrome, min(available, 52))
		// Past the margin the mirror keeps every row it can: it is watched for
		// what is happening now, and that is at the bottom of the screen.
		return min(available, width), max(10, m.height-2*paneWindowVMargin)
	}
	width := min(140, max(52, m.width*17/20))
	height := min(46, max(18, m.height*17/20))
	return min(m.width, width), min(m.height, height)
}

func (m *Model) rebuildPreviewLayout() {
	session := m.previewedSession()
	if session == nil {
		m.previewLayout = nil
		m.previewLayoutWidth = 0
		return
	}
	width := m.quickLookContentWidth()
	m.previewLayout = m.buildPreviewLines(*session, width)
	m.previewLayoutWidth = width
}

func (m *Model) quickLookContentWidth() int {
	width, _ := m.quickLookDimensions()
	if m.paneView {
		frame := m.paneFrame()
		if !frame.floats() {
			return max(20, width)
		}
		return max(20, width-frame.chrome)
	}
	return max(20, width-paneWindowChrome)
}

// quickLookBodyRow is the terminal row the mirror's first body line lands on:
// the header, the subtitle and the rule above it.
const quickLookBodyRow = 3

// boardTopRow is the terminal row the board starts on: the header line and its
// bottom border.
const boardTopRow = 2

// cursorRow maps the pane's cursor onto this terminal, given the slice of the
// capture currently on screen. It reports -1 when the pane hides its cursor, or
// when the row it sits on is scrolled out of the mirror — a cursor drawn at the
// wrong place is worse than none, since it is the only thing telling a typist
// where their keystrokes are going.
func (m *Model) cursorRow(start, end int) int {
	if !m.paneView || !m.paneCursor.CursorVisible {
		return -1
	}
	// The cursor is reported against the visible screen, which sits below
	// whatever scrollback the capture brought along.
	row := m.paneCursor.History + m.paneCursor.CursorY
	if row < start || row >= end {
		return -1
	}
	// The first and last visible rows are given over to the scroll markers.
	screenRow := quickLookBodyRow + row - start
	if (start > 0 && row == start) || (end < len(m.paneLines) && row == end-1) {
		return -1
	}
	return screenRow
}

// paneBodyLines crops the captured screen to the overlay. The capture is
// already laid out for the pane's own width, so cropping is all that can be
// done to it — and it is done ANSI-aware, or a cut in the middle of a colour
// escape would bleed that colour across the rest of the overlay.
func (m *Model) paneBodyLines(width int) []string {
	if m.paneErr != nil {
		return []string{
			lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F87171")).
				Render("Unable to read the pane"),
			mutedStyle.Render(m.paneErr.Error()),
		}
	}
	if len(m.paneLines) == 0 {
		if m.previewLoading {
			return []string{"Reading pane…"}
		}
		return []string{"The pane is empty."}
	}
	lines := make([]string, len(m.paneLines))
	for i, line := range m.paneLines {
		lines[i] = truncate(line, width)
	}
	return lines
}

// trimBlankTail drops the empty rows at the bottom of a capture. A pane is
// usually taller than the overlay, so the overlay shows the end of the capture
// — and an agent that leaves styling on the last row of its screen makes tmux
// keep every blank row above it, which would fill the overlay with nothing and
// push the conversation out of sight.
func trimBlankTail(lines []string, keep int) []string {
	end := len(lines)
	for end > max(0, keep) && strings.TrimSpace(ansi.Strip(lines[end-1])) == "" {
		end--
	}
	return lines[:min(end, len(lines))]
}

func widestLine(lines []string) int {
	widest := 0
	for _, line := range lines {
		widest = max(widest, lipgloss.Width(line))
	}
	return widest
}

func (m *Model) sessionByID(id string) *agent.Session {
	for i := range m.sessions {
		if m.sessions[i].ID == id {
			return &m.sessions[i]
		}
	}
	return nil
}

func (m *Model) buildPreviewLines(
	session agent.Session,
	width int,
) []string {
	if m.previewLoading {
		return []string{"Loading conversation…"}
	}
	if m.previewErr != nil {
		return []string{
			lipgloss.NewStyle().
				Foreground(lipgloss.Color("#F87171")).
				Render("Unable to load preview"),
			mutedStyle.Render(m.previewErr.Error()),
		}
	}
	if len(m.previewMessages) == 0 {
		return []string{"No conversation messages found."}
	}

	// Wrapping is the expensive half of a layout pass, and a live session only
	// ever changes its newest message, so the lines of every message that
	// survived the poll unchanged are carried over instead of recomputed.
	previous := m.previewWrapped
	if m.previewWrapWidth != width {
		previous = nil
	}
	wrapped := make(map[string][]string, len(m.previewMessages))

	lines := make([]string, 0)
	for _, message := range m.previewMessages {
		label := strings.ToUpper(session.Agent)
		labelColor := "#60A5FA"
		if message.Role == agent.RoleUser {
			label = "YOU"
			labelColor = "#C4B5FD"
		}
		if !message.Timestamp.IsZero() {
			label += " · " + message.Timestamp.Local().Format("15:04")
		}
		lines = append(lines, lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(labelColor)).
			Render(label))

		body, reused := previous[message.Text]
		if !reused {
			body = strings.Split(lipgloss.Wrap(
				strings.ReplaceAll(message.Text, "\r\n", "\n"),
				width,
				" ",
			), "\n")
		}
		wrapped[message.Text] = body
		lines = append(lines, body...)
		lines = append(lines, "")
	}
	m.previewWrapped = wrapped
	m.previewWrapWidth = width
	return lines
}

func (m *Model) renderDetail() string {
	s := m.selected()
	if s == nil {
		return lipgloss.JoinVertical(
			lipgloss.Left,
			m.renderHeader(),
			m.renderBoard(),
			m.renderFooter(),
		)
	}
	width := min(76, max(30, m.width-8))
	detail := []string{
		lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F8FAFC")).
			Render(truncate(s.Title, width-6)),
		"",
		"Agent       " + s.Agent,
		"Status      " + string(s.RuntimeStatus),
		"Project     " + projectName(s.CWD),
		"Workspace   " + s.CWD,
		"Branch      " + fallback(s.Branch, "—"),
		"Source      " + sourceLabel(*s),
		"Updated     " + s.UpdatedAt.Format(time.RFC1123),
		"Tokens      " + formatNumber(s.TokensUsed),
		"Session ID  " + s.ID,
	}
	detail = append(detail, "", mutedStyle.Render("esc close · enter close"))
	box := lipgloss.NewStyle().
		Width(width).
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#A78BFA")).
		Background(lipgloss.Color("#0F172A")).
		Foreground(lipgloss.Color("#CBD5E1")).
		Padding(1, 2).
		Render(strings.Join(detail, "\n"))
	boxWidth, boxHeight := lipgloss.Width(box), lipgloss.Height(box)
	m.detailRect = screenRect{
		x:      max(0, (m.width-boxWidth)/2),
		y:      max(0, (m.height-boxHeight)/2),
		width:  boxWidth,
		height: boxHeight,
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func sourceLabel(s agent.Session) string {
	if s.AgentNickname != "" {
		role := s.AgentRole
		if role == "" {
			role = "sub-agent"
		}
		return s.AgentNickname + " · " + role
	}
	if len(s.Source) > 40 {
		return truncate(s.Source, 40)
	}
	return s.Source
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}

func formatNumber(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// truncate fits a value into width display cells. It walks the string once:
// every card on the board and every line of the Quick Look chrome is truncated
// on each frame, and measuring the whole string once per dropped rune made a
// long CJK title cost more than the rest of the frame put together.
func truncate(value string, width int) string {
	value = strings.ReplaceAll(value, "\n", " ")
	if width <= 1 {
		if lipgloss.Width(value) <= width {
			return value
		}
		return "…"
	}
	return ansi.Truncate(value, width, "…")
}

func fallback(value, empty string) string {
	if value == "" {
		return empty
	}
	return value
}

func intersperse(values []string, separator string) []string {
	if len(values) < 2 {
		return values
	}
	result := make([]string, 0, len(values)*2-1)
	for i, value := range values {
		if i > 0 {
			result = append(result, separator)
		}
		result = append(result, value)
	}
	return result
}
