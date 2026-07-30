package handlers_rds

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// The window machinery both scheduled passes share. A backup window is a UTC
// time of day and a maintenance window is a UTC day and time of day, but
// everything around them is the same: AWS's textual form, a minimum length, a
// deterministic assignment when the customer names none, and the instant the
// current window opened — which is what lets a pass fire exactly once per window
// from a persisted stamp rather than from a timer's memory.

const (
	oneDay  = 24 * time.Hour
	oneWeek = 7 * oneDay

	// AWS's granularity and minimum length for both windows. An assigned window
	// is exactly one slot, which is also the shortest one a customer may name.
	windowSlot      = 30 * time.Minute
	minWindowLength = 30 * time.Minute

	// AWS's own abbreviations, lowercase as it returns them.
	weekdayNames = "sun mon tue wed thu fri sat"
)

// A UTC time-of-day window in AWS's hh24:mi-hh24:mi form, held as offsets from
// midnight. An end at or before the start wraps midnight, which AWS allows and a
// deterministic assignment near the end of a block produces.
type dailyWindow struct {
	start, end time.Duration
}

// A UTC weekly window in AWS's ddd:hh24:mi-ddd:hh24:mi form, held as offsets
// from Sunday midnight so a window spanning a day boundary needs no special
// case. Maintenance is weekly, so the day is part of the window rather than
// something the caller supplies alongside it.
type weeklyWindow struct {
	start, end time.Duration
}

func parseDailyWindow(field, value string) (dailyWindow, error) {
	from, to, err := cutWindow(field, value)
	if err != nil {
		return dailyWindow{}, err
	}
	start, err := parseClock(field, from)
	if err != nil {
		return dailyWindow{}, err
	}
	end, err := parseClock(field, to)
	if err != nil {
		return dailyWindow{}, err
	}
	window := dailyWindow{start: start, end: end}
	if window.length() < minWindowLength {
		return dailyWindow{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s must be at least %s long", field, minWindowLength)
	}
	return window, nil
}

func parseWeeklyWindow(field, value string) (weeklyWindow, error) {
	from, to, err := cutWindow(field, value)
	if err != nil {
		return weeklyWindow{}, err
	}
	start, err := parseWeekdayClock(field, from)
	if err != nil {
		return weeklyWindow{}, err
	}
	end, err := parseWeekdayClock(field, to)
	if err != nil {
		return weeklyWindow{}, err
	}
	window := weeklyWindow{start: start, end: end}
	if window.length() < minWindowLength {
		return weeklyWindow{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s must be at least %s long", field, minWindowLength)
	}
	return window, nil
}

// The two halves of a window, rejecting anything that is not exactly one
// hyphen-separated pair — a value with none or several is malformed rather than
// something to guess at.
func cutWindow(field, value string) (string, string, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return "", "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s %q is malformed; the form is %s", field, value, windowForm(field))
	}
	return parts[0], parts[1], nil
}

// Offsets from midnight, from AWS's hh24:mi.
func parseClock(field, value string) (time.Duration, error) {
	hour, minute, ok := strings.Cut(value, ":")
	if !ok || !fixedDigits(hour, 2) || !fixedDigits(minute, 2) {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s %q is malformed; the form is %s", field, value, windowForm(field))
	}
	hours, _ := strconv.Atoi(hour)
	minutes, _ := strconv.Atoi(minute)
	if hours > 23 || minutes > 59 {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s %q is not a UTC time of day", field, value)
	}
	return time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute, nil
}

// Offsets from Sunday midnight, from AWS's ddd:hh24:mi.
func parseWeekdayClock(field, value string) (time.Duration, error) {
	day, clock, ok := strings.Cut(value, ":")
	if !ok {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s %q is malformed; the form is %s", field, value, windowForm(field))
	}
	index := strings.Index(weekdayNames, strings.ToLower(day))
	// Four characters per name, so a match that is not on a name boundary is a
	// substring of one rather than a day.
	if len(day) != 3 || index < 0 || index%4 != 0 {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s day %q is not a weekday; use one of %s", field, day, strings.ReplaceAll(weekdayNames, " ", ", "))
	}
	offset, err := parseClock(field, clock)
	if err != nil {
		return 0, err
	}
	return time.Duration(index/4)*oneDay + offset, nil
}

func fixedDigits(value string, want int) bool {
	if len(value) != want {
		return false
	}
	for _, r := range value {
		if !isDigit(r) {
			return false
		}
	}
	return true
}

func windowForm(field string) string {
	if strings.Contains(field, "Maintenance") {
		return "ddd:hh24:mi-ddd:hh24:mi in UTC"
	}
	return "hh24:mi-hh24:mi in UTC"
}

// A window that wraps is measured forward through midnight, so an equal start
// and end reads as zero rather than a whole day and fails the minimum length.
func (w dailyWindow) length() time.Duration {
	return (w.end - w.start + oneDay) % oneDay
}

func (w weeklyWindow) length() time.Duration {
	return (w.end - w.start + oneWeek) % oneWeek
}

func (w dailyWindow) contains(t time.Time) bool {
	return withinSegments(w.segments(), sinceMidnight(t))
}

func (w weeklyWindow) contains(t time.Time) bool {
	return withinSegments(w.segments(), sinceSunday(t))
}

// The most recent instant this window opened at or before t. Only meaningful
// while t is inside the window, which is the only place the passes call it: it is
// what a persisted stamp is compared against to decide whether this window has
// already fired.
func (w dailyWindow) openedAt(t time.Time) time.Time {
	opened := midnight(t).Add(w.start)
	if opened.After(t) {
		opened = opened.Add(-oneDay)
	}
	return opened
}

func (w weeklyWindow) openedAt(t time.Time) time.Time {
	opened := midnight(t).Add(-time.Duration(t.UTC().Weekday()) * oneDay).Add(w.start)
	if opened.After(t) {
		opened = opened.Add(-oneWeek)
	}
	return opened
}

func (w dailyWindow) String() string {
	return formatClock(w.start) + "-" + formatClock(w.end)
}

func (w weeklyWindow) String() string {
	return formatWeekdayClock(w.start) + "-" + formatWeekdayClock(w.end)
}

// Whether the two windows share any time of day. The maintenance window is
// projected onto a single day for the comparison, which is what AWS's own
// non-overlap rule does: it rejects an overlapping pair whatever day the
// maintenance window falls on, because the two would collide the week they meet.
func (w dailyWindow) overlaps(other weeklyWindow) bool {
	for _, a := range w.segments() {
		for _, b := range other.timeOfDay().segments() {
			if a[0] < b[1] && b[0] < a[1] {
				return true
			}
		}
	}
	return false
}

// The window's daily projection. A window a whole day or longer covers every
// time of day, so it is reported as one that leaves no gap.
func (w weeklyWindow) timeOfDay() dailyWindow {
	if w.length() >= oneDay {
		return dailyWindow{start: 0, end: oneDay}
	}
	return dailyWindow{start: w.start % oneDay, end: w.end % oneDay}
}

// A wrapping window as the two non-wrapping spans it covers, so an overlap or a
// containment check is plain interval arithmetic rather than a case analysis.
func (w dailyWindow) segments() [][2]time.Duration {
	if w.start < w.end {
		return [][2]time.Duration{{w.start, w.end}}
	}
	return [][2]time.Duration{{w.start, oneDay}, {0, w.end}}
}

func (w weeklyWindow) segments() [][2]time.Duration {
	if w.start < w.end {
		return [][2]time.Duration{{w.start, w.end}}
	}
	return [][2]time.Duration{{w.start, oneWeek}, {0, w.end}}
}

func withinSegments(segments [][2]time.Duration, offset time.Duration) bool {
	for _, segment := range segments {
		if offset >= segment[0] && offset < segment[1] {
			return true
		}
	}
	return false
}

func sinceMidnight(t time.Time) time.Duration {
	t = t.UTC()
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second
}

func sinceSunday(t time.Time) time.Duration {
	return time.Duration(t.UTC().Weekday())*oneDay + sinceMidnight(t)
}

func midnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func formatClock(offset time.Duration) string {
	offset = (offset%oneDay + oneDay) % oneDay
	return fmt.Sprintf("%02d:%02d", int(offset/time.Hour), int(offset/time.Minute)%60)
}

func formatWeekdayClock(offset time.Duration) string {
	offset = (offset%oneWeek + oneWeek) % oneWeek
	day := int(offset / oneDay)
	return weekdayNames[day*4:day*4+3] + ":" + formatClock(offset%oneDay)
}

// A window derived from the DB instance identifier rather than chosen at random.
// AWS assigns a random one, which here would move whenever a record is rewritten;
// a hash over the configured block is stable for the life of the instance and
// still spreads the fleet's quiesce load across it.
func assignDailyWindow(block dailyWindow, identifier string) dailyWindow {
	return assignDailySlot(block, identifier, 0)
}

// The assignment stepped on by whole slots through the block, wrapping at its
// end. Stepping is what lets an assigned window move off one the customer named
// rather than colliding with it.
func assignDailySlot(block dailyWindow, identifier string, shift int64) dailyWindow {
	slots := int64(block.length() / windowSlot)
	if slots < 1 {
		return block
	}
	slot := int64(windowHash(identifier) % uint64(slots)) //nolint:gosec // the modulus bounds it to the slot count
	start := (block.start + time.Duration((slot+shift)%slots)*windowSlot) % oneDay
	return dailyWindow{start: start, end: (start + windowSlot) % oneDay}
}

// The same assignment plus a day, seeded apart from the backup window's so an
// instance does not land on correlated positions within the two blocks.
func assignWeeklyWindow(block dailyWindow, identifier string) weeklyWindow {
	return assignWeeklySlot(block, identifier, 0)
}

func assignWeeklySlot(block dailyWindow, identifier string, shift int64) weeklyWindow {
	daily := assignDailySlot(block, identifier, shift)
	day := int64(windowHash(identifier+"/maintenance") % 7)
	start := (time.Duration(day)*oneDay + daily.start) % oneWeek
	return weeklyWindow{start: start, end: (start + windowSlot) % oneWeek}
}

// The assigned backup window stepped clear of the maintenance window in force.
// A window the customer did not name must never be the reason their request is
// rejected, so the assignment moves — which is what AWS does with the window it
// assigned itself. A block every slot of which collides falls back to the slot
// beginning where the other window ends, so the pair is still separable when the
// operator's block sits inside it.
func assignDailyWindowClearOf(block dailyWindow, identifier string, avoid weeklyWindow) dailyWindow {
	for shift := range int64(block.length() / windowSlot) {
		if window := assignDailySlot(block, identifier, shift); !window.overlaps(avoid) {
			return window
		}
	}
	return slotAfter(avoid.timeOfDay().end)
}

func assignWeeklyWindowClearOf(block dailyWindow, identifier string, avoid dailyWindow) weeklyWindow {
	for shift := range int64(block.length() / windowSlot) {
		if window := assignWeeklySlot(block, identifier, shift); !avoid.overlaps(window) {
			return window
		}
	}
	day := int64(windowHash(identifier+"/maintenance") % 7)
	daily := slotAfter(avoid.end)
	start := (time.Duration(day)*oneDay + daily.start) % oneWeek
	return weeklyWindow{start: start, end: (start + windowSlot) % oneWeek}
}

// The one slot beginning at offset. Whether it is free is the caller's overlap
// check to make: for a window covering all but half an hour of the day there is
// no free slot to find, and that pair is reported rather than placed.
func slotAfter(offset time.Duration) dailyWindow {
	start := (offset%oneDay + oneDay) % oneDay
	return dailyWindow{start: start, end: (start + windowSlot) % oneDay}
}

func windowHash(value string) uint64 {
	h := fnv.New64a()
	// Hash.Write never returns an error, which is why the interface's error is
	// documented as always nil.
	_, _ = h.Write([]byte(value))
	return h.Sum64()
}
