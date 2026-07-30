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
	"github.com/Jewel591/openagentview/internal/prefs"
	"github.com/Jewel591/openagentview/internal/tmux"
)

var benchmarkRenderedQuickLook string
var benchmarkPreviewLayout []string

type previewAdapter struct {
	messages []agent.TranscriptMessage
	status   agent.RuntimeStatus
	activity agent.Activity
	calls    int
	limits   []int
}

func (a *previewAdapter) Name() string {
	return "codex"
}

func (a *previewAdapter) Discover(context.Context, int) ([]agent.Session, error) {
	return nil, nil
}

func (a *previewAdapter) Preview(
	_ context.Context,
	_ agent.Session,
	limit int,
) (agent.Transcript, error) {
	a.calls++
	a.limits = append(a.limits, limit)
	start := max(0, len(a.messages)-limit)
	return agent.Transcript{
		Messages: append([]agent.TranscriptMessage(nil), a.messages[start:]...),
		Status:   a.status,
		Activity: a.activity,
	}, nil
}

func (a *previewAdapter) ResumeCommand(agent.Session) (string, []string) {
	return "", nil
}

type fakePanes struct {
	lines     []string
	history   []string
	cursorX   int
	cursorY   int
	cursor    bool
	alternate bool
	width     int
	err       error
	sent      []string
	captured  []int
}

func (p *fakePanes) Capture(
	_ context.Context, _ string, history int,
) (tmux.Screen, error) {
	p.captured = append(p.captured, history)
	screen := tmux.Screen{
		Lines:           append([]string(nil), p.lines...),
		CursorX:         p.cursorX,
		CursorY:         p.cursorY,
		CursorVisible:   p.cursor,
		AlternateScreen: p.alternate,
		Width:           p.width,
		HistorySize:     len(p.history),
	}
	if history > 0 {
		included := min(history, len(p.history))
		screen.Lines = append(
			append([]string(nil), p.history[len(p.history)-included:]...),
			p.lines...,
		)
		screen.History = included
	}
	return screen, p.err
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

// A session running in a pane has a screen, and the screen shows what the
// rollout log cannot: the prompt the agent is currently blocked on.
func TestQuickLookMirrorsTheTmuxPaneOfALiveSession(t *testing.T) {
	panes := &fakePanes{lines: []string{"› approve shell command? [y/N]"}}
	m := tmuxModel(panes)

	load := m.openQuickLook()
	if !m.paneView {
		t.Fatal("Quick Look on a tmux session did not mirror its pane")
	}
	// The overlay opens browsing, so the space that opened it can close it
	// again; answering the agent is behind i.
	if m.paneInput {
		t.Fatal("the mirrored pane opened typing instead of browsing")
	}
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

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

// The resting poll carries only the screen. Leaving it freezes the mirror like
// terminal copy mode, fetches history in bounded pages, and snaps back to a
// fresh live screen when the scroll returns.
func TestMirrorScrollReachesTheScrollback(t *testing.T) {
	history := make([]string, 1200)
	for i := range history {
		history[i] = "history-" + strconv.Itoa(i)
	}
	panes := &fakePanes{
		lines:   []string{"now"},
		history: history,
	}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	if panes.captured[0] != 0 {
		t.Fatalf("the opening capture asked for %d rows of history, want none",
			panes.captured[0])
	}

	// A poll capture dispatched before the crossing is still in flight; its
	// screen-only frame must die with its generation rather than land later
	// and briefly replace the history being read.
	stale := m.loadPane(m.previewSession, m.previewGeneration, true)

	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if cmd == nil {
		t.Fatal("crossing into scrollback did not refresh the mirror")
	}
	msg, ok := cmd().(paneLoadedMsg)
	if !ok {
		t.Fatalf("scroll produced %T, want paneLoadedMsg", cmd())
	}
	_, nextPoll := m.Update(msg)
	if nextPoll != nil {
		t.Fatal("a scrolled mirror kept its high-frequency live poll running")
	}
	if last := panes.captured[len(panes.captured)-1]; last != paneHistoryPageLines {
		t.Fatalf("scrolled capture asked for %d rows, want %d",
			last, paneHistoryPageLines)
	}
	wantOldest := history[len(history)-paneHistoryPageLines]
	if m.paneLines[0] != wantOldest {
		t.Fatalf("paneLines start with %q, want %q", m.paneLines[0], wantOldest)
	}
	if !strings.Contains(m.View().Content, history[len(history)-4]) {
		t.Fatal("the scrolled mirror did not show the scrollback")
	}

	_, _ = m.Update(stale().(paneLoadedMsg))
	if m.paneLines[0] != wantOldest {
		t.Fatal("a stale screen-only frame replaced the scrollback")
	}

	// The pane keeps printing while the reader is up in the history; the
	// offset grows with the content so the rows under their eyes hold still.
	panes.lines = []string{"now", "brand-new"}
	before := m.previewScrollBack
	session := m.previewedSession()
	next, ok := m.loadPane(*session, m.previewGeneration, false)().(paneLoadedMsg)
	if !ok {
		t.Fatal("reload did not produce a paneLoadedMsg")
	}
	_, _ = m.Update(next)
	if m.previewScrollBack != before+1 {
		t.Fatalf("scrollback offset = %d, want %d to hold the reader's place",
			m.previewScrollBack, before+1)
	}

	// Movement inside the captured page stays local; only returning to the
	// bottom deserves an immediate live screen and a restarted poll.
	for m.previewScrollBack > wheelScrollLines {
		if _, cmd = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown}); cmd != nil {
			t.Fatal("scrolling within the captured history refreshed the mirror")
		}
	}
	_, cmd = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if cmd == nil {
		t.Fatal("returning to the live edge did not refresh the mirror")
	}
	if back, ok := cmd().(paneLoadedMsg); ok {
		_, _ = m.Update(back)
	}
	if last := panes.captured[len(panes.captured)-1]; last != 0 {
		t.Fatalf("the live capture asked for %d rows of history, want none", last)
	}
}

// The automatic hop from the pane into the transcript keeps the pane
// window's exact size in both directions: a window that changes shape
// mid-scroll reads as a flicker, not a handoff.
func TestTranscriptContinuationKeepsThePaneWindowSize(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "screen-" + strconv.Itoa(i)
	}
	messages := make([]agent.TranscriptMessage, 40)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role: agent.RoleAgent,
			Text: "transcript-" + strconv.Itoa(i),
		}
	}
	panes := &fakePanes{lines: lines, alternate: true, width: 80}
	m := tmuxModel(panes)
	m.adapter = &previewAdapter{messages: messages}
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	paneWidth, paneHeight := m.quickLookDimensions()

	m.previewScrollBack = m.previewMaxScrollBack()
	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.paneView {
		t.Fatal("scrolling past the pane boundary did not enter the transcript")
	}
	if w, h := m.quickLookDimensions(); w != paneWidth || h != paneHeight {
		t.Fatalf("continuation resized the window to %dx%d, want %dx%d",
			w, h, paneWidth, paneHeight)
	}
	// The size holds after the transcript actually arrives, not just in the
	// loading gap.
	_, _ = m.Update(cmd().(previewLoadedMsg))
	if w, h := m.quickLookDimensions(); w != paneWidth || h != paneHeight {
		t.Fatalf("the loaded transcript resized the window to %dx%d, want %dx%d",
			w, h, paneWidth, paneHeight)
	}

	// The way back is the same window too.
	m.previewScrollBack = 0
	_, cmd = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if !m.paneView {
		t.Fatal("scrolling through the transcript bottom did not return live")
	}
	if back, ok := cmd().(paneLoadedMsg); ok {
		_, _ = m.Update(back)
	}
	if w, h := m.quickLookDimensions(); w != paneWidth || h != paneHeight {
		t.Fatalf("returning live resized the window to %dx%d, want %dx%d",
			w, h, paneWidth, paneHeight)
	}
}

// Alternate-screen TUIs give tmux only their current frame. Reaching the top
// continues into the agent-owned transcript instead of presenting that tmux
// boundary as the end of the conversation.
func TestMirrorScrollContinuesPastAnAlternateScreenIntoTranscript(t *testing.T) {
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "screen-" + strconv.Itoa(i)
	}
	messages := make([]agent.TranscriptMessage, 40)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role: agent.RoleAgent,
			Text: "transcript-" + strconv.Itoa(i),
		}
	}
	// A realistic pane width: the continuation window keeps the pane's frame,
	// and a zero-width pane would leave no room for the subtitle under test.
	panes := &fakePanes{lines: lines, alternate: true, width: 100}
	m := tmuxModel(panes)
	adapter := &previewAdapter{messages: messages}
	m.adapter = adapter
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	// Typing when the pane overflows into the transcript, so the return trip
	// has a typing state worth restoring.
	press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	if !m.paneInput {
		t.Fatal("i did not start typing into the pane")
	}

	maxScroll := m.previewMaxScrollBack()
	if maxScroll <= 0 {
		t.Fatal("test pane does not extend past the Quick Look body")
	}
	m.previewScrollBack = maxScroll
	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.paneView {
		t.Fatal("scrolling past the pane boundary did not enter the transcript")
	}
	if m.paneInput {
		t.Fatal("the transcript kept forwarding keys to the pane")
	}
	if !m.previewTranscriptReturnsToPane {
		t.Fatal("the automatic transcript did not remember its live-pane edge")
	}
	transcriptMessage := cmd()
	transcript, ok := transcriptMessage.(previewLoadedMsg)
	if !ok {
		t.Fatalf("pane overflow produced %T, want previewLoadedMsg", transcriptMessage)
	}
	if transcript.limit != previewMessagePage {
		t.Fatalf("first transcript page = %d messages, want %d",
			transcript.limit, previewMessagePage)
	}
	_, _ = m.Update(transcript)
	if m.previewScrollBack == 0 {
		t.Fatal("the boundary wheel was lost while the transcript loaded")
	}
	if panes.captured[len(panes.captured)-1] != 0 {
		t.Fatal("an alternate screen invented tmux history")
	}
	view := m.View().Content
	if !strings.Contains(view, "session transcript") ||
		!strings.Contains(view, "live pane continues below") {
		t.Fatal("the automatic source change was not explained")
	}

	_, cmd = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if !m.paneView {
		t.Fatal("scrolling through the transcript bottom did not return live")
	}
	if !m.paneInput {
		t.Fatal("returning live did not restore the pane's typing state")
	}
	liveMessage := cmd()
	if _, ok := liveMessage.(paneLoadedMsg); !ok {
		t.Fatalf("live return produced %T, want paneLoadedMsg", liveMessage)
	}
}

func TestMirrorWithAShortAlternateScreenContinuesImmediately(t *testing.T) {
	m := tmuxModel(&fakePanes{
		lines:     []string{"the whole current screen"},
		alternate: true,
	})
	m.adapter = &previewAdapter{messages: []agent.TranscriptMessage{
		{Role: agent.RoleUser, Text: strings.Repeat("older request ", 20)},
		{Role: agent.RoleAgent, Text: strings.Repeat("older answer ", 20)},
	}}
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.paneView {
		t.Fatal("a zero-overflow pane swallowed the first history gesture")
	}
	loaded := cmd()
	if _, ok := loaded.(previewLoadedMsg); !ok {
		t.Fatalf("short pane overflow produced %T, want previewLoadedMsg", loaded)
	}
}

func TestTranscriptLoadsOlderMessagesOnlyWhenItsTopIsCrossed(t *testing.T) {
	messages := make([]agent.TranscriptMessage, 80)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role: agent.RoleAgent,
			Text: "message-" + strconv.Itoa(i),
		}
	}
	adapter := &previewAdapter{messages: messages}
	m := tmuxModel(&fakePanes{})
	m.adapter = adapter
	m.sessions[0].TmuxPane = ""
	m.sessions[0].TmuxTarget = ""

	load := m.openQuickLook()
	first := loadMsg(t, load).(previewLoadedMsg)
	_, _ = m.Update(first)
	if len(m.previewMessages) != previewMessagePage {
		t.Fatalf("initial transcript messages = %d, want %d",
			len(m.previewMessages), previewMessagePage)
	}

	m.previewScrollBack = m.previewMaxScrollBack()
	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	secondMessage := cmd()
	second, ok := secondMessage.(previewLoadedMsg)
	if !ok {
		t.Fatalf("first transcript expansion produced %T", secondMessage)
	}
	if second.limit != previewMessagePage*2 {
		t.Fatalf("first expanded limit = %d, want %d",
			second.limit, previewMessagePage*2)
	}
	_, duplicate := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if duplicate != nil {
		t.Fatal("trackpad inertia launched a duplicate transcript expansion")
	}
	if m.previewMessageLimit != previewMessagePage*2 {
		t.Fatalf("in-flight expansion grew again to %d", m.previewMessageLimit)
	}
	if !strings.Contains(m.View().Content, "loading older history") {
		t.Fatal("an in-flight transcript expansion had no visible feedback")
	}
	_, _ = m.Update(second)
	if len(m.previewMessages) != previewMessagePage*2 {
		t.Fatalf("expanded transcript messages = %d, want %d",
			len(m.previewMessages), previewMessagePage*2)
	}
	start := max(
		0,
		len(m.previewLayout)-m.quickLookBodyHeight()-m.previewScrollBack,
	)
	if start == 0 {
		t.Fatal("expanding older messages jumped to the oldest row of the new page")
	}

	m.previewScrollBack = m.previewMaxScrollBack()
	_, cmd = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	third := cmd().(previewLoadedMsg)
	_, _ = m.Update(third)
	if len(adapter.limits) != 3 ||
		adapter.limits[0] != 16 ||
		adapter.limits[1] != 32 ||
		adapter.limits[2] != 64 {
		t.Fatalf("Preview limits = %v, want [16 32 64]", adapter.limits)
	}
}

func TestTranscriptStopsRebuildingOnceTheOldestMessageIsKnown(t *testing.T) {
	messages := make([]agent.TranscriptMessage, 20)
	for i := range messages {
		messages[i] = agent.TranscriptMessage{
			Role: agent.RoleAgent,
			Text: "message-" + strconv.Itoa(i),
		}
	}
	adapter := &previewAdapter{messages: messages}
	m := tmuxModel(&fakePanes{})
	m.adapter = adapter
	m.sessions[0].TmuxPane = ""
	m.sessions[0].TmuxTarget = ""

	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(previewLoadedMsg))
	m.previewScrollBack = m.previewMaxScrollBack()
	_, cmd := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	_, _ = m.Update(cmd().(previewLoadedMsg))
	if !m.previewTranscriptExhausted {
		t.Fatal("a short expanded page did not mark the transcript exhausted")
	}

	m.previewScrollBack = m.previewMaxScrollBack()
	_, cmd = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if cmd != nil {
		t.Fatal("scroll inertia rebuilt a transcript whose oldest message is known")
	}
	if adapter.calls != 2 {
		t.Fatalf("Preview calls = %d, want initial load plus one expansion", adapter.calls)
	}
}

func TestMirrorCanReachHistoryPastTwoThousandLines(t *testing.T) {
	history := make([]string, 5000)
	for i := range history {
		history[i] = "deep-" + strconv.Itoa(i)
	}
	panes := &fakePanes{lines: []string{"now"}, history: history}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	cmd := press(t, m, tea.KeyPressMsg{Code: 'g', Text: "g"})
	if cmd == nil {
		t.Fatal("going to the oldest history did not expand the capture")
	}
	msg := cmd().(paneLoadedMsg)
	_, nextPoll := m.Update(msg)
	if nextPoll != nil {
		t.Fatal("the oldest-history view restarted the live poll")
	}
	if last := panes.captured[len(panes.captured)-1]; last != len(history) {
		t.Fatalf("oldest capture asked for %d rows, want all %d",
			last, len(history))
	}
	if m.paneLines[0] != history[0] {
		t.Fatalf("oldest captured line = %q, want %q",
			m.paneLines[0], history[0])
	}
}

// The view-only note explains a state where the key hints only name keys, so
// when the activity line of a live session squeezes the footer, the note must
// survive the width that drops the hints.
func TestViewOnlyNoteOutlivesTheKeyHints(t *testing.T) {
	m := tmuxModel(&fakePanes{})
	m.sessions[0].TmuxPane = ""
	m.sessions[0].TmuxTarget = ""
	m.openQuickLook()
	m.previewLoading = false
	m.previewStatus = agent.StatusRunning
	m.previewActivity = agent.Activity{Label: "calling Read"}

	narrow := m.renderQuickLookFooter(60)
	if !strings.Contains(narrow, "view only (not in tmux)") {
		t.Fatalf("narrow footer lost the view-only note: %q", narrow)
	}
	if strings.Contains(narrow, "pgup") {
		t.Fatalf("key hints outranked the note at a width that fits only one: %q", narrow)
	}

	wide := m.renderQuickLookFooter(160)
	if !strings.Contains(wide, "view only (not in tmux)") ||
		!strings.Contains(wide, "pgup") {
		t.Fatalf("a wide footer should carry the note and the hints: %q", wide)
	}
}

// A live session mirrored from its pane can be typed into, so its transcript
// continuation must not claim to be view-only.
func TestMirrorableTranscriptCarriesNoViewOnlyNote(t *testing.T) {
	m := tmuxModel(&fakePanes{})
	m.openQuickLook()
	m.continuePaneIntoTranscript(1)
	m.previewLoading = false
	m.previewStatus = agent.StatusRunning

	footer := m.renderQuickLookFooter(160)
	if strings.Contains(footer, "view only") {
		t.Fatalf("a mirrorable session was marked view-only: %q", footer)
	}
	if !strings.Contains(footer, "returns live pane") {
		t.Fatalf("the transcript continuation should offer the way back: %q", footer)
	}
}

func TestTypingIntoAPaneSendsTextAndReturnSeparately(t *testing.T) {
	panes := &fakePanes{lines: []string{"waiting"}}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})

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

// The mirror follows the macOS Quick Look rhythm: space opens browsing and
// space closes again. Typing sits behind i, where every key — space and enter
// included — belongs to the agent, and esc steps back out to browsing.
func TestSpaceTogglesTheMirrorAndTypingSitsBehindI(t *testing.T) {
	panes := &fakePanes{lines: []string{"waiting"}}
	m := tmuxModel(panes)
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	if !m.paneInput {
		t.Fatal("i did not start typing into the pane")
	}

	// While typing, spaces are text for the agent, not the close gesture.
	for _, key := range []tea.KeyPressMsg{
		{Code: ' ', Text: " "},
		{Code: 'a', Text: "a"},
		{Code: ' ', Text: " "},
	} {
		cmd := press(t, m, key)
		if cmd == nil {
			t.Fatalf("%q did not reach the pane", key.Text)
		}
		cmd()
	}
	if !m.previewOpen || !m.paneInput {
		t.Fatal("typed spaces closed Quick Look instead of reaching the agent")
	}
	if len(panes.sent) != 3 || !strings.HasSuffix(panes.sent[2], "text: ") {
		t.Fatalf("sent %v, want three typed characters", panes.sent)
	}

	// Esc leaves typing without closing the window or reaching the agent.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.paneInput {
		t.Fatal("esc did not stop typing")
	}
	if !m.previewOpen {
		t.Fatal("esc closed Quick Look instead of stopping typing")
	}
	if len(panes.sent) != 3 {
		t.Fatalf("sent %v, want the esc kept from the agent", panes.sent)
	}

	// ctrl+] stays as an alias for the hands already trained on it.
	press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	press(t, m, tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl})
	if m.paneInput || !m.previewOpen {
		t.Fatal("ctrl+] did not step back out to browsing")
	}

	// Browsing again, the space that opened the window closes it.
	press(t, m, tea.KeyPressMsg{Code: ' ', Text: " "})
	if m.previewOpen {
		t.Fatal("space did not close Quick Look from browse mode")
	}
	if len(panes.sent) != 3 {
		t.Fatalf("sent %v, want the closing space kept out of the pane", panes.sent)
	}
}

// A click inside the mirror is the mouse's way of pressing i; a click outside
// stays the mouse's way of putting the window down.
func TestClickOnTheMirrorStartsTyping(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"waiting"}, width: 80})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))
	// A render records where the window sits, which is what a click resolves
	// against.
	_ = m.View()

	x := m.quickLookRect.x + m.quickLookRect.width/2
	y := m.quickLookRect.y + m.quickLookRect.height/2
	_, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if !m.paneInput {
		t.Fatal("a click on the mirror did not start typing")
	}
	if !m.previewOpen {
		t.Fatal("a click on the mirror closed it")
	}

	_, _ = m.Update(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	if m.previewOpen || m.paneInput {
		t.Fatal("a click outside did not put the window down")
	}
}

// The transcript is reached by scrolling, not by a mode key: t stopped being
// a view toggle when the scroll continuation covered it.
func TestTIsNotAViewToggle(t *testing.T) {
	m := tmuxModel(&fakePanes{lines: []string{"live screen"}})
	load := m.openQuickLook()
	_, _ = m.Update(loadMsg(t, load).(paneLoadedMsg))

	press(t, m, tea.KeyPressMsg{Code: 't', Text: "t"})
	if !m.paneView {
		t.Fatal("t switched views instead of doing nothing")
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

	// Browsing is not typing: the cursor would claim keys go somewhere they do
	// not.
	if m.View().Cursor != nil {
		t.Fatal("the cursor was on before typing started")
	}

	press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})
	cursor := m.View().Cursor
	if cursor == nil {
		t.Fatal("the mirror showed no cursor while typing into the agent")
	}
	if cursor.X != 2 || cursor.Y != quickLookBodyRow {
		t.Fatalf("cursor = %d,%d, want 2,%d", cursor.X, cursor.Y, quickLookBodyRow)
	}

	press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
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
	press(t, m, tea.KeyPressMsg{Code: 'i', Text: "i"})

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

// Grouped by status no heading names the project, so each row carries its
// own project column; grouped by project the heading already says it.
func TestStatusGroupedListRowsNameTheirProject(t *testing.T) {
	m := listModel()
	m.sessions[0].CWD = "/projects/mono"
	m.sessions[0].Title = "Fix the parser"

	board := ansi.Strip(m.renderBoard())
	statusRow := ""
	for _, line := range strings.Split(board, "\n") {
		if strings.Contains(line, "Fix the parser") {
			statusRow = line
		}
	}
	if statusRow == "" {
		t.Fatal("the session's row did not render")
	}
	if !strings.Contains(statusRow, "mono") {
		t.Fatalf("status-grouped row does not name its project: %q", statusRow)
	}
	if strings.Index(statusRow, "mono") > strings.Index(statusRow, "Fix the parser") {
		t.Fatalf("the project column should sit before the title: %q", statusRow)
	}

	m.group = groupProject
	board = ansi.Strip(m.renderBoard())
	for _, line := range strings.Split(board, "\n") {
		if strings.Contains(line, "Fix the parser") && strings.Contains(line, "mono") {
			t.Fatalf("project-grouped row repeats the heading's project: %q", line)
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
	if got := listDescription(session, true); got != "codex · mono · fix/parser" {
		t.Fatalf("description = %q, want the session's location", got)
	}
	// Grouped by project, the heading above the row already names it.
	if got := listDescription(session, false); got != "codex · fix/parser" {
		t.Fatalf("description = %q, want the location without the project", got)
	}
	session.Preview = "Fix the parser, then add a regression test"
	if got := listDescription(session, true); got != session.Preview {
		t.Fatalf("description = %q, want the prompt", got)
	}
}

// A board with more projects than the width can hold shows fewer columns at
// a readable width instead of squeezing them all in, and slides the window
// to keep the selected column on screen.
func TestKanbanKeepsColumnsReadableAndScrollsTheOverflow(t *testing.T) {
	now := time.Now()
	m := &Model{group: groupProject, width: 120, height: 40}
	for i := 0; i < 9; i++ {
		name := "project-" + string(rune('a'+i))
		m.sessions = append(m.sessions, agent.Session{
			ID:        name,
			Agent:     "codex",
			Title:     "task in " + name,
			CWD:       "/projects/" + name,
			RecencyAt: now,
			UpdatedAt: now,
		})
	}
	m.clampSelection()

	countVisible := func() int {
		board := ansi.Strip(m.renderBoard())
		visible := 0
		for i := 0; i < 9; i++ {
			if strings.Contains(board, "project-"+string(rune('a'+i))+" ") {
				visible++
			}
		}
		return visible
	}

	visible := countVisible()
	if visible >= 9 {
		t.Fatal("a 120-cell board squeezed all nine columns in")
	}
	if visible < 2 {
		t.Fatalf("visible columns = %d, want at least two", visible)
	}

	// The last column is off-screen until the selection travels there.
	board := ansi.Strip(m.renderBoard())
	if strings.Contains(board, "project-i") {
		t.Fatal("the last column was visible before the selection reached it")
	}
	m.column = 8
	m.clampSelection()
	if !strings.Contains(ansi.Strip(m.renderBoard()), "project-i") {
		t.Fatal("the window did not slide to keep the selected column visible")
	}
}

// The board comes back arranged the way it was left: grouping, layout and
// the agent filter all survive one model being torn down and another built
// on the same state file.
func TestBoardArrangementSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	store, err := prefs.OpenAt(path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	m := tmuxModel(&fakePanes{})
	m.preferences = store
	m.toggleGroup()
	m.toggleLayout()
	m.toggleAgentFilter("codex")

	reopened, err := prefs.OpenAt(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	next := tmuxModel(&fakePanes{})
	next.preferences = reopened
	next.applyPrefs()
	if next.group != groupProject {
		t.Fatal("the grouping did not survive a restart")
	}
	if next.layout != layoutList {
		t.Fatal("the layout did not survive a restart")
	}
	if len(next.agentFilter) != 1 || !next.agentFilter["codex"] {
		t.Fatalf("agentFilter = %v, want codex alone", next.agentFilter)
	}

	// Toggling everything back leaves the file at the defaults again.
	next.toggleGroup()
	next.toggleLayout()
	next.toggleAgentFilter("codex")
	final, err := prefs.OpenAt(path)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	saved := final.Load()
	if saved.Group != "status" || saved.Layout != "kanban" || len(saved.Agents) != 0 {
		t.Fatalf("saved = %+v, want the defaults", saved)
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
	m := &Model{
		previewOpen: true,
		paneView:    true,
		paneInput:   true,
		paneLoaded:  true,
		paneLines: []string{
			"one", "two", "three", "four", "five",
			"six", "seven", "eight", "nine", "ten",
		},
	}

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

// The composer is the board's standing input: describe a task, and it starts
// as a fresh agent in a tmux session of its own.

type fakeStarter struct {
	name    string
	dir     string
	command []string
	width   int
	height  int
	err     error
}

func (s *fakeStarter) NewSession(
	_ context.Context,
	name, dir string,
	command []string,
	width, height int,
) (string, error) {
	s.name, s.dir, s.command = name, dir, command
	s.width, s.height = width, height
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
	// On this 120×40 board the largest pane the floating mirror shows whole is
	// 110×26: left to tmux the detached session would be 80×24, and its
	// mirror a small screen adrift in a mostly empty overlay.
	if starter.width != 110 || starter.height != 26 {
		t.Fatalf("pane size = %d×%d, want 110×26 to fill the floating mirror",
			starter.width, starter.height)
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

type fakeOpener struct {
	dir     string
	command []string
	err     error
	calls   int
}

func (f *fakeOpener) OpenTab(dir string, command []string) error {
	f.calls++
	f.dir, f.command = dir, command
	return f.err
}

type resumableAdapter struct{ previewAdapter }

func (resumableAdapter) ResumeCommand(s agent.Session) (string, []string) {
	return "claude", []string{"--resume", s.ID}
}

func tabModel(opener TabOpener) *Model {
	now := time.Now()
	return &Model{
		adapter: &resumableAdapter{},
		opener:  opener,
		width:   120,
		height:  40,
		sessions: []agent.Session{
			{
				ID: "idle-one", Agent: "claude", Title: "Idle",
				CWD: "/projects/alpha", RuntimeStatus: agent.StatusIdle,
				UpdatedAt: now, RecencyAt: now,
			},
			{
				ID: "pane-one", Agent: "codex", Title: "In a pane",
				CWD: "/projects/beta", RuntimeStatus: agent.StatusRunning,
				UpdatedAt: now, RecencyAt: now, PID: 42, TmuxPane: "%7",
			},
		},
	}
}

func TestEnterOpensQuickLook(t *testing.T) {
	m := tabModel(&fakeOpener{})
	m.column = 2
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.previewOpen {
		t.Fatal("enter on the board did not open Quick Look")
	}
}

func TestCtrlEnterResumesAnIdleSessionInATab(t *testing.T) {
	opener := &fakeOpener{}
	m := tabModel(opener)
	m.column = 2

	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+enter produced no command")
	}
	if msg := cmd().(tabOpenedMsg); msg.err != nil {
		t.Fatalf("opening the tab failed: %v", msg.err)
	}
	if opener.dir != "/projects/alpha" {
		t.Fatalf("tab opened in %q, want the session's directory", opener.dir)
	}
	want := []string{"claude", "--resume", "idle-one"}
	if len(opener.command) != len(want) || opener.command[0] != want[0] ||
		opener.command[1] != want[1] || opener.command[2] != want[2] {
		t.Fatalf("tab runs %v, want the agent's resume command", opener.command)
	}
}

func TestCtrlEnterAttachesToAPaneSessionInATab(t *testing.T) {
	opener := &fakeOpener{}
	m := tabModel(opener)
	m.column = 1

	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	if msg := cmd().(tabOpenedMsg); msg.err != nil {
		t.Fatalf("opening the tab failed: %v", msg.err)
	}
	if opener.command[0] != "tmux" {
		t.Fatalf("tab runs %v, want a tmux command", opener.command)
	}
	joined := strings.Join(opener.command, " ")
	// A fresh tab's shell is never inside tmux, so the way in is attach.
	if !strings.Contains(joined, "attach -t %7") || strings.Contains(joined, "switch-client") {
		t.Fatalf("tab runs %q, want an attach to the session's pane", joined)
	}
	// An attach cares about the pane, not the path: a workspace gone stale
	// since must not stop a perfectly attachable session at the cd.
	if opener.dir != "" {
		t.Fatalf("attach was given directory %q, want none", opener.dir)
	}
}

func TestCtrlEnterRefusesASessionOpenElsewhere(t *testing.T) {
	opener := &fakeOpener{}
	m := tabModel(opener)
	m.sessions[1].TmuxPane = ""
	m.column = 1

	if cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl}); cmd != nil {
		t.Fatal("a session open in another terminal still produced a command")
	}
	if opener.calls != 0 {
		t.Fatal("the opener was asked for a tab anyway")
	}
	if !strings.Contains(m.status, "another terminal") {
		t.Fatalf("status %q does not say why nothing happened", m.status)
	}
}

func TestCtrlEnterFallsBackWithoutARecognizedTerminal(t *testing.T) {
	m := tabModel(nil)
	m.column = 2
	if cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("without an opener ctrl+enter did not fall back to resuming in place")
	}
}

func TestQuickLookEnterOpensTheSessionInATab(t *testing.T) {
	opener := &fakeOpener{}
	m := tabModel(opener)
	m.column = 1
	m.previewOpen = true

	cmd := press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.previewOpen {
		t.Fatal("enter left Quick Look open")
	}
	if cmd == nil {
		t.Fatal("Quick Look enter produced no command")
	}
	cmd()
	if opener.calls != 1 {
		t.Fatalf("the opener was called %d times, want once", opener.calls)
	}
}

func TestWheelLeavesTheBoardSelectionAlone(t *testing.T) {
	// A trackpad's inertia fires dozens of wheel events per flick; mapped to
	// the selection, a stray swipe sent it flying. Only Quick Look scrolls.
	m := clickBoardModel()
	column, row := m.column, m.row

	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.column != column || m.row != row {
		t.Fatalf("wheel moved the selection from column %d row %d to column %d row %d",
			column, row, m.column, m.row)
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

func TestPasteLandsInTheComposer(t *testing.T) {
	starter := &fakeStarter{}
	m := composerModelWithProjects(starter)

	press(t, m, tea.KeyPressMsg{Code: 'n', Text: "n"})
	m.Update(tea.PasteMsg{Content: "fix the flaky\r\nretry loop\tin @al"})

	if m.composeText != "fix the flaky retry loop in @al" {
		t.Fatalf("paste landed as %q", m.composeText)
	}
	// A pasted @token is a live mention like a typed one.
	if got := m.composeMenuEntries(); len(got) != 1 || got[0] != "/projects/alpha" {
		t.Fatalf("menu after pasting a mention offered %v", got)
	}
}

func TestPasteLandsInTheSearchQuery(t *testing.T) {
	m := composerModelWithProjects(&fakeStarter{})

	press(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	m.Update(tea.PasteMsg{Content: "retry\nloop"})

	if !m.searching || m.query != "retry loop" {
		t.Fatalf("paste while searching left searching=%v query=%q", m.searching, m.query)
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

func TestRunningCardsHoldTheirOrderAcrossRefreshes(t *testing.T) {
	now := time.Now()
	running := func(id string, created, updated time.Time) agent.Session {
		return agent.Session{
			ID:            id,
			Agent:         "claude",
			CWD:           "/projects/alpha",
			RuntimeStatus: agent.StatusRunning,
			CreatedAt:     created,
			UpdatedAt:     updated,
			RecencyAt:     updated,
		}
	}
	m := &Model{sessions: []agent.Session{
		running("older-start", now.Add(-2*time.Hour), now),
		running("newer-start", now.Add(-time.Hour), now.Add(-time.Second)),
	}}

	order := func() []string {
		var ids []string
		for _, card := range m.cardsForColumn(1) {
			ids = append(ids, card.ID)
		}
		return ids
	}
	first := order()
	if len(first) != 2 || first[0] != "newer-start" {
		t.Fatalf("running cards sorted %v, want the newer session first", first)
	}

	// The next poll sees the other session write last. Order is a session's
	// place on the board, not a race between their logs — it must not move.
	m.sessions[0].UpdatedAt = now.Add(-time.Second)
	m.sessions[1].UpdatedAt = now.Add(time.Second)
	if second := order(); second[0] != first[0] || second[1] != first[1] {
		t.Fatalf("a refresh reordered running cards from %v to %v", first, second)
	}
}

func TestProjectColumnsLeadWithWhatWantsAPerson(t *testing.T) {
	now := time.Now()
	m := &Model{group: groupProject, sessions: []agent.Session{
		{
			ID: "settled", Agent: "claude", CWD: "/projects/alpha",
			RuntimeStatus: agent.StatusIdle,
			CreatedAt:     now.Add(-time.Minute), UpdatedAt: now, RecencyAt: now,
		},
		{
			ID: "working", Agent: "claude", CWD: "/projects/alpha",
			RuntimeStatus: agent.StatusRunning,
			CreatedAt:     now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Minute),
			RecencyAt: now.Add(-time.Minute),
		},
		{
			ID: "waiting", Agent: "claude", CWD: "/projects/alpha",
			RuntimeStatus: agent.StatusNeedsYou,
			CreatedAt:     now.Add(-3 * time.Hour), UpdatedAt: now.Add(-time.Hour),
			RecencyAt: now.Add(-time.Hour),
		},
	}}

	cards := m.cardsForColumn(0)
	if len(cards) != 3 {
		t.Fatalf("project column holds %d cards, want 3", len(cards))
	}
	if cards[0].ID != "waiting" || cards[1].ID != "working" || cards[2].ID != "settled" {
		t.Fatalf("project column ordered %s, %s, %s — want needs-you, running, idle",
			cards[0].ID, cards[1].ID, cards[2].ID)
	}
}

func TestHelpFooterHeightMatchesItsLines(t *testing.T) {
	m := &Model{helpOpen: true, width: 200}
	if got := m.footerHeight(); got != len(m.shortcutHelpLines())+1 || got != 2 {
		t.Fatalf("wide help takes %d rows, want 2", got)
	}
	m.width = 80
	if got := m.footerHeight(); got != len(m.shortcutHelpLines())+1 || got != 3 {
		t.Fatalf("narrow help takes %d rows, want 3", got)
	}
	m.helpOpen = false
	if m.footerHeight() != 1 {
		t.Fatal("the closed footer is one row")
	}
}

func TestRunningPulseArmsAndStandsDown(t *testing.T) {
	now := time.Now()
	running := agent.Session{
		ID: "r", Agent: "claude", CWD: "/projects/alpha",
		RuntimeStatus: agent.StatusRunning,
		CreatedAt:     now, UpdatedAt: now, RecencyAt: now,
	}
	m := &Model{layout: layoutList}

	// A refresh that finds a running session arms the pulse; the next
	// refresh must not stack a second timer on top of it.
	_, cmd := m.Update(refreshMsg{sessions: []agent.Session{running}})
	if !m.animating || cmd == nil {
		t.Fatalf("refresh with a running session left animating=%v cmd=%v", m.animating, cmd)
	}
	if _, cmd = m.Update(refreshMsg{sessions: []agent.Session{running}}); cmd != nil {
		t.Fatal("a second refresh armed a second pulse timer")
	}

	// Each tick advances the frame and re-arms itself.
	frame := m.animFrame
	_, cmd = m.Update(animTickMsg(time.Now()))
	if m.animFrame != frame+1 || cmd == nil {
		t.Fatalf("tick advanced frame %d→%d with cmd=%v", frame, m.animFrame, cmd)
	}

	// Kanban never draws the marker, so switching away stands the pulse
	// down — animating a layout that cannot show it is redraw for nothing.
	m.layout = layoutKanban
	if _, cmd = m.Update(animTickMsg(time.Now())); m.animating || cmd != nil {
		t.Fatalf("tick on kanban left animating=%v cmd=%v", m.animating, cmd)
	}

	// Switching back to the list brings the pulse with it.
	press(t, m, tea.KeyPressMsg{Code: 'v', Text: "v"})
	if m.layout != layoutList || !m.animating {
		t.Fatalf("v back to the list left layout=%v animating=%v", m.layout, m.animating)
	}

	// With nothing running anymore the pulse stands down instead of
	// spinning an idle board forever.
	m.sessions[0].RuntimeStatus = agent.StatusIdle
	if _, cmd = m.Update(animTickMsg(time.Now())); m.animating || cmd != nil {
		t.Fatalf("tick with nothing running left animating=%v cmd=%v", m.animating, cmd)
	}
}

func TestRunningPulseStaysOffTheKanban(t *testing.T) {
	now := time.Now()
	m := &Model{}
	_, cmd := m.Update(refreshMsg{sessions: []agent.Session{{
		ID: "r", Agent: "claude", CWD: "/projects/alpha",
		RuntimeStatus: agent.StatusRunning,
		CreatedAt:     now, UpdatedAt: now, RecencyAt: now,
	}}})
	if m.animating || cmd != nil {
		t.Fatalf("kanban armed the pulse: animating=%v cmd=%v", m.animating, cmd)
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

// A column that has the rows for two cards shows two: counting the gap after
// the last card as spent would scroll a board that fits.
func TestColumnShowsEveryCardThatFits(t *testing.T) {
	now := time.Now()
	m := &Model{
		group:    groupStatus,
		layout:   layoutKanban,
		width:    120,
		lastSync: now,
		sessions: []agent.Session{
			{ID: "a", Title: "first", RuntimeStatus: agent.StatusRunning, RecencyAt: now},
			{ID: "b", Title: "second", RuntimeStatus: agent.StatusRunning, RecencyAt: now},
		},
	}
	// One header row plus two cards and the gap between them.
	board := m.renderColumn(1, m.columns()[1], 40, 1+2*cardHeight+1, 0, boardTopRow)
	for _, title := range []string{"first", "second"} {
		if !strings.Contains(board, title) {
			t.Fatalf("column dropped %q with room for it:\n%s", title, board)
		}
	}
}

// Grouped by project an empty board has no columns at all, and must still say
// why it is empty rather than render as a blank rectangle.
func TestEmptyProjectBoardExplainsItself(t *testing.T) {
	m := &Model{
		group:    groupProject,
		layout:   layoutKanban,
		width:    120,
		height:   40,
		lastSync: time.Now(),
	}
	if board := m.renderBoard(); !strings.Contains(board, "No agents running.") {
		t.Fatalf("empty project board = %q, want the empty state", board)
	}
}

// The first discovery takes up to a few seconds; until it lands the board has
// found nothing, which is not the same as there being nothing.
func TestBoardWaitsForTheFirstScanBeforeSayingItIsEmpty(t *testing.T) {
	m := &Model{group: groupStatus, layout: layoutKanban, width: 120, height: 40}
	board := m.renderBoard()
	if strings.Contains(board, "No agents running.") {
		t.Fatalf("board called itself empty before the first scan:\n%s", board)
	}
	if !strings.Contains(board, "Looking for sessions") {
		t.Fatalf("board = %q, want the pre-scan state", board)
	}
}

// agentFilterModel is three idle sessions, one per agent, all recent enough
// that only the agent filter can take any of them off the board.
func agentFilterModel() *Model {
	now := time.Now()
	return &Model{
		group:  groupStatus,
		width:  160,
		height: 40,
		sessions: []agent.Session{
			{ID: "c1", Agent: "claude", Title: "task alpha", RuntimeStatus: agent.StatusIdle, RecencyAt: now},
			{ID: "x1", Agent: "codex", Title: "task beta", RuntimeStatus: agent.StatusIdle, RecencyAt: now},
			{ID: "g1", Agent: "grok", Title: "task gamma", RuntimeStatus: agent.StatusIdle, RecencyAt: now},
		},
	}
}

func idleIDs(m *Model) []string {
	ids := []string{}
	for _, session := range m.cardsForColumn(2) {
		ids = append(ids, session.ID)
	}
	return ids
}

func TestNoAgentChipSelectedShowsEveryAgent(t *testing.T) {
	m := agentFilterModel()
	if got := idleIDs(m); len(got) != 3 {
		t.Fatalf("unfiltered board = %v, want all three sessions", got)
	}
}

func TestAgentChipsToggleIndependently(t *testing.T) {
	m := agentFilterModel()

	// Chips are drawn in a stable order, so the first is claude.
	m.toggleAgentAt(0)
	if got := idleIDs(m); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("claude filter = %v, want only the claude session", got)
	}

	// A second chip widens the filter rather than replacing the first.
	m.toggleAgentAt(2)
	got := idleIDs(m)
	if len(got) != 2 || got[0] != "c1" || got[1] != "g1" {
		t.Fatalf("claude+grok filter = %v, want both sessions", got)
	}

	// Turning the last lit chip off is the same as never having filtered.
	m.toggleAgentAt(0)
	m.toggleAgentAt(2)
	if got := idleIDs(m); len(got) != 3 {
		t.Fatalf("emptied filter = %v, want all three sessions", got)
	}
}

func TestAgentFilterNarrowsSearchRatherThanBeingOverriddenByIt(t *testing.T) {
	m := agentFilterModel()
	m.toggleAgentFilter("codex")
	m.query = "task"
	if got := idleIDs(m); len(got) != 1 || got[0] != "x1" {
		t.Fatalf("filtered search = %v, want only the codex session", got)
	}
}

func TestDigitTogglesAnAgentChipAndClearsWithA(t *testing.T) {
	m := agentFilterModel()
	m.clampSelection()

	press(t, m, tea.KeyPressMsg{Code: '2', Text: "2"})
	if got := idleIDs(m); len(got) != 1 || got[0] != "x1" {
		t.Fatalf("after 2 = %v, want only the codex session", got)
	}

	press(t, m, tea.KeyPressMsg{Code: 'a', Text: "a"})
	if got := idleIDs(m); len(got) != 3 {
		t.Fatalf("after a = %v, want all three sessions", got)
	}
}

// A digit past the end of the chip run is a key pressed at a chip that is not
// there, which must not blank the board.
func TestDigitPastTheLastAgentChipChangesNothing(t *testing.T) {
	m := agentFilterModel()
	m.toggleAgentAt(7)
	if got := idleIDs(m); len(got) != 3 {
		t.Fatalf("out-of-range chip = %v, want all three sessions", got)
	}
}

// A chip stays drawn while it is lit, even once its agent's last session
// leaves the board — otherwise the only switch that could clear the filter
// would vanish with it.
func TestALitAgentChipSurvivesItsAgentLeavingTheBoard(t *testing.T) {
	m := agentFilterModel()
	m.toggleAgentFilter("grok")
	m.sessions = m.sessions[:2]
	names := m.agentNames()
	if len(names) != 3 || names[2] != "grok" {
		t.Fatalf("agent chips = %v, want grok kept", names)
	}
}

func TestASingleAgentGetsNoChips(t *testing.T) {
	m := agentFilterModel()
	m.sessions = m.sessions[:1]
	if chips, widths := m.renderAgentChips(false); chips != "" || widths != nil {
		t.Fatalf("chips with one agent = %q/%v, want none", chips, widths)
	}
}

// An input method that composes text out of the letter keys eats q, so the
// board has to be leavable without switching it off.
func TestCtrlQQuitsTheBoard(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: 'q', Text: "q"},
		{Code: 'q', Mod: tea.ModCtrl},
		{Code: 'c', Mod: tea.ModCtrl},
	} {
		m := agentFilterModel()
		m.clampSelection()
		if cmd := press(t, m, key); cmd == nil {
			t.Fatalf("%v returned no command, want quit", key)
		}
	}
}

// Quit has to work from the overlays too: the input method that swallows q on
// the board swallows it just as thoroughly with Quick Look open.
func TestCtrlQQuitsFromTheOverlays(t *testing.T) {
	for name, open := range map[string]func(*Model){
		"quick look": func(m *Model) { m.previewOpen = true },
		"help":       func(m *Model) { m.helpOpen = true },
		"detail":     func(m *Model) { m.detail = true },
	} {
		m := agentFilterModel()
		m.clampSelection()
		open(m)
		if cmd := press(t, m, tea.KeyPressMsg{Code: 'q', Mod: tea.ModCtrl}); cmd == nil {
			t.Fatalf("ctrl+q with %s open returned no command, want quit", name)
		}
	}
}

// Filtering redraws the columns under the cursor, so the selection follows the
// session rather than the row it happened to be on.
func TestTogglingAnAgentKeepsTheSelectedSession(t *testing.T) {
	m := agentFilterModel()
	m.toggleAgentFilter("grok")
	m.clampSelection()
	selected := m.selected()
	if selected == nil || selected.ID != "g1" {
		t.Fatalf("selected = %#v, want the grok session", selected)
	}

	// Lighting claude puts a card ahead of the grok one in the same column.
	m.toggleAgentFilter("claude")
	if got := m.selected(); got == nil || got.ID != "g1" {
		t.Fatalf("after widening the filter selected = %#v, want g1 still", got)
	}
}

// An empty board under a filter is the filter's doing, and saying "no agents
// running" would be a finding the board did not make.
func TestAnEmptyFilteredBoardNamesTheFilterAndTheWayOut(t *testing.T) {
	m := agentFilterModel()
	m.lastSync = time.Now()
	m.sessions[2].RecencyAt = time.Now().Add(-48 * time.Hour)
	m.toggleAgentFilter("grok")

	board := m.renderEmptyBoard(12)
	for _, want := range []string{"GROK", "show every agent"} {
		if !strings.Contains(board, want) {
			t.Fatalf("empty filtered board = %q, want it to mention %q", board, want)
		}
	}
	if strings.Contains(board, "No agents running") {
		t.Fatal("empty filtered board blamed the agents, not the filter")
	}
}

// The chips are the newest thing on the header, so they give way before the
// search hint they were added alongside.
func TestANarrowHeaderKeepsTheSearchHintOverTheChips(t *testing.T) {
	m := agentFilterModel()
	m.width = 80

	header := m.renderHeader()
	if !strings.Contains(header, "/ search") {
		t.Fatalf("80-column header = %q, want the search hint kept", header)
	}
	// Every recorded zone has to land on screen, chips included.
	for _, zone := range m.clickZones {
		if zone.rect.x+zone.rect.width > m.width {
			t.Fatalf("zone %#v runs past the %d-column header", zone, m.width)
		}
	}

	// At 80 columns the tabs and the hint leave no room for chips at all, so
	// the lit agent is said in the hint instead of vanishing with them.
	m.toggleAgentFilter("codex")
	m.clickZones = nil
	header = m.renderHeader()
	if !strings.Contains(header, "CODEX") || !strings.Contains(header, "/ search") {
		t.Fatalf("80-column filtered header = %q, want the lit agent named and the hint kept", header)
	}
	if strings.Contains(header, "last 24h") {
		t.Fatalf("80-column filtered header = %q, want the filter said where last 24h was", header)
	}
}

// The narrow header's fallback hint has its own width budget: naming three
// agents where one fit would push the search entry off the line it was kept
// on in the first place.
func TestTheNarrowHeaderHintFitsWhateverItSays(t *testing.T) {
	for _, lit := range [][]string{{"codex"}, {"claude", "codex"}, {"claude", "codex", "grok"}} {
		m := agentFilterModel()
		m.width = 80
		for _, name := range lit {
			m.toggleAgentFilter(name)
		}
		header := m.renderHeader()
		if !strings.Contains(header, "/ search") {
			t.Fatalf("80-column header with %v lit = %q, want the search entry kept", lit, header)
		}
		for _, zone := range m.clickZones {
			if zone.rect.x+zone.rect.width > m.width {
				t.Fatalf("zone %#v runs past the %d-column header with %v lit", zone, m.width, lit)
			}
		}
	}
}

// A search inside a filter is still inside it, and a header that has dropped
// its chips has to say so or the missing results look like the query's doing.
func TestTheNarrowHeaderSaysTheFilterAlongsideAQuery(t *testing.T) {
	m := agentFilterModel()
	m.width = 80
	m.toggleAgentFilter("codex")
	m.query = "task"

	header := m.renderHeader()
	if !strings.Contains(header, "CODEX") || !strings.Contains(header, "filter: task") {
		t.Fatalf("80-column header = %q, want both the agent and the query named", header)
	}
}

// Clearing the filter has to actually produce the cards the empty board
// promises, and an archived session is one no column will take.
func TestTheEmptyBoardDoesNotPromiseArchivedSessions(t *testing.T) {
	m := agentFilterModel()
	m.lastSync = time.Now()
	m.sessions = []agent.Session{
		{ID: "g1", Agent: "grok", RuntimeStatus: agent.StatusIdle, RecencyAt: time.Now().Add(-48 * time.Hour)},
		{ID: "c1", Agent: "claude", RuntimeStatus: agent.StatusArchived, Archived: true, RecencyAt: time.Now()},
	}
	m.toggleAgentFilter("grok")

	if held := m.heldBackByAgentFilter(); held != 0 {
		t.Fatalf("held back = %d, want 0: the only other session is archived", held)
	}
	board := m.renderEmptyBoard(12)
	if strings.Contains(board, "waiting behind the filter") {
		t.Fatalf("empty board = %q, want it not to promise cards clearing the filter cannot produce", board)
	}
}

// The count the empty board does give has to be the number of cards clearing
// the filter puts back.
func TestTheEmptyBoardCountsTheCardsClearingTheFilterWouldShow(t *testing.T) {
	m := agentFilterModel()
	m.lastSync = time.Now()
	m.sessions = append(m.sessions, agent.Session{
		ID: "old", Agent: "claude", RuntimeStatus: agent.StatusIdle,
		RecencyAt: time.Now().Add(-48 * time.Hour),
	})
	// grok is lit but its only session is stale, so the board under the
	// filter is empty while claude and codex still have a card each.
	m.sessions[2].RecencyAt = time.Now().Add(-48 * time.Hour)
	m.toggleAgentFilter("grok")

	if held := m.heldBackByAgentFilter(); held != 2 {
		t.Fatalf("held back = %d, want the 2 recent sessions of the other agents", held)
	}
}
