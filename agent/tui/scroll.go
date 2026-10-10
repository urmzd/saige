package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// ── Motion ──────────────────────────────────────────────────────────

// fadeFor is how long a new entry is drawn faint before it settles.
const fadeFor = 300 * time.Millisecond

// scrollFrame is the interval between the frames of a smooth scroll.
const scrollFrame = 16 * time.Millisecond

// MotionEnabled reports whether the TUI may animate: not when noAnimation
// is set, when NO_COLOR or SAIGE_REDUCED_MOTION is set, or on a dumb
// terminal. Without motion the spinner is a fixed glyph, new entries appear
// at once, and scrolling jumps instead of easing.
func MotionEnabled(noAnimation bool) bool {
	if noAnimation {
		return false
	}
	for _, env := range []string{"NO_COLOR", "SAIGE_REDUCED_MOTION"} {
		if os.Getenv(env) != "" {
			return false
		}
	}
	return os.Getenv("TERM") != "dumb"
}

// staticSpinner stands in for the spinner when motion is off.
const staticSpinner = "•"

// ── Scrolling ───────────────────────────────────────────────────────

// scrollTickMsg advances a smooth scroll by one frame.
type scrollTickMsg struct{}

func scrollTick() tea.Cmd {
	return tea.Tick(scrollFrame, func(time.Time) tea.Msg { return scrollTickMsg{} })
}

// scrollAction is one way to move through the transcript.
type scrollAction int

const (
	scrollNone scrollAction = iota
	scrollLineUp
	scrollLineDown
	scrollHalfUp
	scrollHalfDown
	scrollPageUp
	scrollPageDown
	scrollTop
	scrollBottom
)

// wheelLines is how far one mouse wheel notch scrolls.
const wheelLines = 3

// scroller is the transcript's viewport. It follows the tail of the log
// while the view is at the bottom; scrolling up pauses that, and reaching
// the bottom again resumes it. While paused it counts what arrived below
// the view, for the "new messages" indicator. With motion on, jumps of
// more than a line ease toward their target over a few frames.
type scroller struct {
	vp      viewport.Model
	animate bool

	follow bool
	// entries is the entry count of the content; seen is the count when
	// following paused, and grew records that lines arrived since.
	entries, seen int
	grew          bool

	moving bool
	target int
}

func newScroller(animate bool) scroller {
	vp := viewport.New(80, 20)
	// The wheel is handled here, so following tracks it.
	vp.MouseWheelEnabled = false
	return scroller{vp: vp, animate: animate, follow: true}
}

// resize sets the view size, keeping the tail in view while following.
func (s *scroller) resize(width, height int) {
	s.vp.Width, s.vp.Height = width, height
	if s.follow {
		s.vp.GotoBottom()
	}
}

// setContent replaces the content, which holds entries entries.
func (s *scroller) setContent(content string, entries int) {
	before := s.vp.TotalLineCount()
	s.vp.SetContent(content)
	s.entries = entries
	if s.follow {
		s.moving = false
		s.vp.GotoBottom()
		s.seen = entries
		return
	}
	if s.vp.TotalLineCount() > before {
		s.grew = true
	}
	if entries < s.seen {
		// A filter changed what is shown; count from here.
		s.seen = entries
	}
	s.target = min(s.target, s.maxOffset())
}

func (s *scroller) maxOffset() int { return max(0, s.vp.TotalLineCount()-s.vp.Height) }

// dest is where the view is headed: the target of a scroll in progress,
// else where it is.
func (s *scroller) dest() int {
	if s.moving {
		return s.target
	}
	return s.vp.YOffset
}

// do performs a scroll action.
func (s *scroller) do(a scrollAction) tea.Cmd {
	half := max(s.vp.Height/2, 1)
	page := max(s.vp.Height, 1)
	switch a {
	case scrollLineUp:
		return s.jumpTo(s.dest() - 1)
	case scrollLineDown:
		return s.jumpTo(s.dest() + 1)
	case scrollHalfUp:
		return s.scrollTo(s.dest() - half)
	case scrollHalfDown:
		return s.scrollTo(s.dest() + half)
	case scrollPageUp:
		return s.scrollTo(s.dest() - page)
	case scrollPageDown:
		return s.scrollTo(s.dest() + page)
	case scrollTop:
		return s.scrollTo(0)
	case scrollBottom:
		return s.scrollTo(s.maxOffset())
	}
	return nil
}

// wheel scrolls for a mouse wheel event; other mouse events do nothing.
func (s *scroller) wheel(msg tea.MouseMsg) {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		s.jumpTo(s.dest() - wheelLines)
	case tea.MouseButtonWheelDown:
		s.jumpTo(s.dest() + wheelLines)
	}
}

// jumpTo moves to off at once.
func (s *scroller) jumpTo(off int) tea.Cmd {
	s.moving = false
	s.settle(off)
	s.vp.SetYOffset(s.target)
	return nil
}

// scrollTo moves to off, easing there when motion is on.
func (s *scroller) scrollTo(off int) tea.Cmd {
	s.settle(off)
	if !s.animate || abs(s.target-s.vp.YOffset) <= 1 {
		s.moving = false
		s.vp.SetYOffset(s.target)
		return nil
	}
	if s.moving {
		return nil // a frame is already scheduled
	}
	s.moving = true
	return scrollTick()
}

// settle records off as the destination and whether it follows the tail.
func (s *scroller) settle(off int) {
	s.target = min(max(off, 0), s.maxOffset())
	wasFollowing := s.follow
	s.follow = s.target >= s.maxOffset()
	switch {
	case s.follow:
		s.seen, s.grew = s.entries, false
	case wasFollowing:
		s.seen, s.grew = s.entries, false
	}
}

// tick advances a smooth scroll by one frame: a third of the remaining
// distance, at least a line, so it slows as it arrives.
func (s *scroller) tick() tea.Cmd {
	if !s.moving {
		return nil
	}
	diff := s.target - s.vp.YOffset
	step := diff / 3
	if step == 0 {
		step = sign(diff)
	}
	s.vp.SetYOffset(s.vp.YOffset + step)
	if s.vp.YOffset == s.target || step == 0 {
		s.moving = false
		return nil
	}
	return scrollTick()
}

// indicator describes what arrived below a paused view, or "" while
// following. key names the key that resumes following.
func (s *scroller) indicator(key string) string {
	if s.follow {
		return ""
	}
	switch n := s.entries - s.seen; {
	case n == 1:
		return fmt.Sprintf("↓ 1 new message (%s follows)", key)
	case n > 1:
		return fmt.Sprintf("↓ %d new messages (%s follows)", n, key)
	case s.grew:
		return fmt.Sprintf("↓ more below (%s follows)", key)
	}
	return ""
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// fade draws s faint while an entry is new.
func fade(s string, born, now time.Time, animate bool) string {
	if age := now.Sub(born); !animate || born.IsZero() || age < 0 || age >= fadeFor || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = fadeStyle.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}
